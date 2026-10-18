// Package board is the swarm's shared memory: a KV store plus an append-only
// decision log. Namespacing is by key prefix ("api/schema", "conv/naming").
package board

import (
	"errors"
	"slices"
	"strings"
	"time"
)

type Entry struct {
	Key     string    `json:"key"`
	Value   string    `json:"value"`
	By      string    `json:"by,omitempty"`
	Time    time.Time `json:"time"`
	Version int       `json:"version"` // 1 on first set, +1 on each overwrite
}

type Decision struct {
	N    int       `json:"n"` // 1-based position in the log
	Text string    `json:"text"`
	By   string    `json:"by,omitempty"`
	Time time.Time `json:"time"`
}

// Board is not safe for concurrent use.
type Board struct {
	kv        map[string]Entry
	decisions []Decision
}

func New() *Board { return &Board{kv: map[string]Entry{}} }

func (b *Board) Set(key, value, by string, t time.Time) (Entry, error) {
	if key == "" {
		return Entry{}, errors.New("board key is required")
	}
	e := Entry{Key: key, Value: value, By: by, Time: t, Version: b.kv[key].Version + 1}
	b.kv[key] = e
	return e, nil
}

func (b *Board) Get(key string) (Entry, bool) {
	e, ok := b.kv[key]
	return e, ok
}

// List returns the entries whose key starts with prefix, sorted by key.
func (b *Board) List(prefix string) []Entry {
	out := []Entry{}
	for k, e := range b.kv {
		if strings.HasPrefix(k, prefix) {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Key, b.Key) })
	return out
}

func (b *Board) Decide(text, by string, t time.Time) (Decision, error) {
	if text == "" {
		return Decision{}, errors.New("decision text is required")
	}
	d := Decision{N: len(b.decisions) + 1, Text: text, By: by, Time: t}
	b.decisions = append(b.decisions, d)
	return d, nil
}

func (b *Board) Decisions() []Decision { return append([]Decision{}, b.decisions...) }
