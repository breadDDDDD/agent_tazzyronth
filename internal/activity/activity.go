// Package activity is the normalized "what is each agent doing" stream (plan
// §3.9): events decoded from workers' JSON output, host hooks, the file diff
// engine and self-reported intent. The Store keeps a ring buffer per agent in
// memory and appends every event to .tazzyronth/activity/<agent>.jsonl; large
// payloads go to activity/blobs/<id>.json and are fetched on demand.
package activity

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Kinds.
const (
	Intent    = "intent"     // self-reported NOW headline (tazzy_progress)
	Say       = "say"        // the agent's visible text
	Read      = "read"       // file read
	Edit      = "edit"       // file write/edit (tool call, or a diff from the watcher)
	Cmd       = "cmd"        // shell command
	CmdResult = "cmd_result" // result of a command, or any failed tool call
	Search    = "search"     // grep/glob/web search
	Tool      = "tool"       // any other tool call
	Msg       = "msg"        // message sent
	Lease     = "lease"      // lease acquired/released
	State     = "state"      // task/agent lifecycle
	Usage     = "usage"      // tokens/cost only
	Alert     = "alert"      // e.g. an edit to a file nobody leased
)

// Sources.
const (
	SrcStream = "stream" // decoded worker stdout (mode A)
	SrcHook   = "hook"   // host hook (mode B)
	SrcDiff   = "diff"   // the hub's file watcher
	SrcHub    = "hub"    // coordination calls seen by the hub
)

// Activity is one normalized event.
type Activity struct {
	ID      int64     `json:"id"` // unique and increasing across the project
	Agent   string    `json:"agent"`
	Task    string    `json:"task,omitempty"`
	TS      time.Time `json:"ts"`
	Kind    string    `json:"kind"`
	Source  string    `json:"source,omitempty"`
	Summary string    `json:"summary"` // one line for the feed
	Files   []string  `json:"files,omitempty"`
	// Detail holds the full args/diff/output when small. Larger payloads are
	// stored separately: Detail is then empty and DetailSize > 0.
	Detail     json.RawMessage `json:"detail,omitempty"`
	DetailSize int             `json:"detail_size,omitempty"`
	Usage      *Tokens         `json:"usage,omitempty"` // added to the agent's tally
}

// Tokens is token usage and the cost the CLI reported (USD, an estimate).
type Tokens struct {
	Input      int64   `json:"input,omitempty"`
	Output     int64   `json:"output,omitempty"`
	CacheRead  int64   `json:"cache_read,omitempty"`
	CacheWrite int64   `json:"cache_write,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
}

func (t *Tokens) add(u Tokens) {
	t.Input += u.Input
	t.Output += u.Output
	t.CacheRead += u.CacheRead
	t.CacheWrite += u.CacheWrite
	t.CostUSD += u.CostUSD
}

// Total is the sum of all token counts.
func (t Tokens) Total() int64 { return t.Input + t.Output + t.CacheRead + t.CacheWrite }

// Totals is the usage tally shown in status.
type Totals struct {
	Agents map[string]Tokens `json:"agents"`
	Swarm  Tokens            `json:"swarm"`
}

const (
	DefaultRing = 500
	inlineMax   = 4 << 10 // details larger than this are stored separately
	summaryMax  = 200
)

// Store holds the activity of a project. It is safe for concurrent use.
type Store struct {
	dir  string
	ring int

	mu     sync.Mutex
	next   int64
	rings  map[string][]Activity
	files  map[string]*os.File
	totals map[string]Tokens
	closed bool
}

// Open loads the activity persisted under dir (the last ring events per
// agent, and the full usage tally) and appends new events there.
func Open(dir string, ring int) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, ring: cmp.Or(ring, DefaultRing), rings: map[string][]Activity{}, files: map[string]*os.File{}, totals: map[string]Tokens{}}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, p := range paths {
		if err := s.load(p); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		var a Activity
		if json.Unmarshal(sc.Bytes(), &a) != nil || a.Agent == "" {
			continue // torn or foreign line: skip
		}
		s.next = max(s.next, a.ID)
		s.remember(a)
	}
	return nil
}

func (s *Store) remember(a Activity) {
	r := append(s.rings[a.Agent], a)
	if len(r) > s.ring {
		r = slices.Delete(r, 0, len(r)-s.ring)
	}
	s.rings[a.Agent] = r
	if a.Usage != nil {
		t := s.totals[a.Agent]
		t.add(*a.Usage)
		s.totals[a.Agent] = t
	}
}

// Add assigns a an id (and a time and one-line summary if needed), stores
// large details separately, persists it and returns the stored event.
func (s *Store) Add(a Activity) (Activity, error) {
	if a.Agent == "" || a.Kind == "" {
		return a, errors.New("activity needs agent and kind")
	}
	if a.TS.IsZero() {
		a.TS = time.Now().UTC()
	}
	a.Summary = OneLine(a.Summary, summaryMax)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return a, errors.New("activity store closed")
	}
	s.next++
	a.ID = s.next
	if len(a.Detail) > inlineMax {
		if err := os.WriteFile(s.blob(a.ID), a.Detail, 0o600); err != nil {
			return a, err
		}
		a.DetailSize, a.Detail = len(a.Detail), nil
	}
	line, err := json.Marshal(a)
	if err != nil {
		return a, err
	}
	f := s.files[a.Agent]
	if f == nil {
		f, err = os.OpenFile(filepath.Join(s.dir, fileName(a.Agent)+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return a, err
		}
		s.files[a.Agent] = f
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return a, err
	}
	s.remember(a)
	return a, nil
}

func (s *Store) blob(id int64) string {
	return filepath.Join(s.dir, "blobs", fmt.Sprintf("%d.json", id))
}

// Recent returns up to limit events (oldest first) with id > after, for one
// agent or, if agent is "", merged across all agents.
func (s *Store) Recent(agent string, after int64, limit int) []Activity {
	s.mu.Lock()
	var out []Activity
	for id, r := range s.rings {
		if agent == "" || id == agent {
			for _, a := range r {
				if a.ID > after {
					out = append(out, a)
				}
			}
		}
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(x, y Activity) int { return cmp.Compare(x.ID, y.ID) })
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	if out == nil {
		out = []Activity{}
	}
	return out
}

// Detail returns the full payload of event id.
func (s *Store) Detail(id int64) (json.RawMessage, error) {
	s.mu.Lock()
	for _, r := range s.rings {
		if i, ok := slices.BinarySearchFunc(r, id, func(a Activity, id int64) int { return cmp.Compare(a.ID, id) }); ok && r[i].DetailSize == 0 {
			d := r[i].Detail
			s.mu.Unlock()
			return d, nil
		}
	}
	s.mu.Unlock()
	b, err := os.ReadFile(s.blob(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no detail for activity %d", id)
	}
	return b, err
}

// Totals returns the usage tally per agent and for the whole swarm.
func (s *Store) Totals() Totals {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := Totals{Agents: map[string]Tokens{}}
	for id, u := range s.totals {
		t.Agents[id] = u
		t.Swarm.add(u)
	}
	return t
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var errs []error
	for _, f := range s.files {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
}

// fileName makes an agent id safe as a file name.
func fileName(agent string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, agent)
}

// OneLine collapses whitespace runs (including newlines) and truncates s to n runes.
func OneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n-1]) + "…"
	}
	return s
}
