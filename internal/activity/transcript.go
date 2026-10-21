package activity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Transcript incrementally tallies the token usage recorded in a Claude Code
// session transcript (a JSONL file, see Hook.Transcript):
//
//	{"type":"assistant","message":{"id":"msg_..","usage":{"input_tokens":..,"output_tokens":..,"cache_read_input_tokens":..,"cache_creation_input_tokens":..}},...}
//
// Lines marked isSidechain are skipped unless the file is a subagent's own
// transcript (agent-<id>.jsonl). One assistant message is often written as
// several lines (one per content block) carrying the same id and usage, so
// each id is counted once; if a later line of the same id reports more, only
// the excess is added. The zero
// value is ready to use; it is not safe for concurrent use.
type Transcript struct {
	Offset int64             // bytes consumed so far (always at a line boundary)
	msgs   map[string]Tokens // message id -> usage counted for it
}

// transcriptMax bounds how much one Read consumes, so a huge transcript seen
// for the first time is tallied over several reads instead of all at once.
const transcriptMax = 32 << 20

// Read consumes the complete lines appended to path since the last call and
// returns the usage they add. A partial last line is left for the next call.
// If the file shrank (rewritten), it is read again from the start.
func (t *Transcript) Read(path string) (Tokens, error) {
	f, err := os.Open(path)
	if err != nil {
		return Tokens{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Tokens{}, err
	}
	if fi.Size() < t.Offset {
		t.Offset, t.msgs = 0, nil
	}
	if fi.Size() == t.Offset {
		return Tokens{}, nil
	}
	if _, err := f.Seek(t.Offset, io.SeekStart); err != nil {
		return Tokens{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, transcriptMax))
	if err != nil {
		return Tokens{}, err
	}
	end := bytes.LastIndexByte(b, '\n')
	if end < 0 {
		if len(b) == transcriptMax {
			t.Offset += int64(len(b)) // one absurd line: skip it
		}
		return Tokens{}, nil
	}
	t.Offset += int64(end + 1)
	// A session transcript may hold its subagents' lines too (isSidechain,
	// older Claude Code versions); those belong to the subagents.
	return t.lines(b[:end+1], IsSubagentTranscript(path)), nil
}

// IsSubagentTranscript reports whether path is a subagent's own transcript,
// agent-<id>.jsonl (see SubagentTranscript), rather than a main session's.
func IsSubagentTranscript(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "agent-") && strings.HasSuffix(base, ".jsonl")
}

// StartOffset is where to start reading a transcript seen for the first time:
// 0 for a subagent's own transcript (born inside the swarm, so all of it
// counts) or a missing file, else the end of its last complete line, so only
// what the session writes from now on is counted. A partial last line is
// left to be read once its newline arrives.
func StartOffset(path string) int64 {
	if IsSubagentTranscript(path) {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	end := fi.Size()
	buf := make([]byte, 64<<10)
	for end > 0 {
		n := int64(len(buf))
		if n > end {
			n = end
		}
		if _, err := f.ReadAt(buf[:n], end-n); err != nil {
			return 0
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return end - n + int64(i) + 1
		}
		end -= n
	}
	return 0
}

func (t *Transcript) lines(b []byte, sidechain bool) Tokens {
	var sum Tokens
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, transcriptMax)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			Sidechain bool   `json:"isSidechain"`
			Message   struct {
				ID    string       `json:"id"`
				Usage *claudeUsage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil || ev.Type != "assistant" || ev.Message.Usage == nil || ev.Sidechain && !sidechain {
			continue
		}
		u := ev.Message.Usage.tokens()
		if ev.Message.ID == "" {
			sum.add(u)
			continue
		}
		if t.msgs == nil {
			t.msgs = map[string]Tokens{}
		}
		old := t.msgs[ev.Message.ID]
		d := Tokens{Input: max(0, u.Input-old.Input), Output: max(0, u.Output-old.Output),
			CacheRead: max(0, u.CacheRead-old.CacheRead), CacheWrite: max(0, u.CacheWrite-old.CacheWrite)}
		if d.Total() == 0 {
			continue
		}
		t.msgs[ev.Message.ID] = Tokens{Input: max(u.Input, old.Input), Output: max(u.Output, old.Output),
			CacheRead: max(u.CacheRead, old.CacheRead), CacheWrite: max(u.CacheWrite, old.CacheWrite)}
		sum.add(d)
	}
	return sum
}

// SubagentTranscript is where Claude Code keeps the transcript of subagent
// agentID of the session whose transcript is session:
// <session dir>/<session id>/subagents/agent-<agentID>.jsonl. If session
// already is that subagent's transcript it is returned as is.
func SubagentTranscript(session, agentID string) string {
	if session == "" || agentID == "" {
		return session
	}
	name := "agent-" + agentID + ".jsonl"
	if filepath.Base(session) == name {
		return session
	}
	return filepath.Join(strings.TrimSuffix(session, filepath.Ext(session)), "subagents", name)
}
