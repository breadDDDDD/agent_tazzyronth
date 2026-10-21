package activity

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestTranscriptRead(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "S.jsonl")
	// Write all but the tail of the last line: it must wait for its newline.
	cut := len(src) - 20
	if err := os.WriteFile(path, src[:cut], 0o644); err != nil {
		t.Fatal(err)
	}
	var tr Transcript
	u, err := tr.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	// msg_a once (split over two lines), the sidechain line skipped.
	if want := (Tokens{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}); u != want {
		t.Fatalf("first read = %+v, want %+v", u, want)
	}
	if u, _ := tr.Read(path); u != (Tokens{}) {
		t.Fatalf("unchanged file read %+v", u)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write(src[cut:])
	// msg_a again with more output (a later, larger report): only the excess.
	f.WriteString(`{"type":"assistant","message":{"id":"msg_a","usage":{"input_tokens":10,"output_tokens":9,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}}}` + "\n")
	f.Close()
	u, _ = tr.Read(path)
	if want := (Tokens{Input: 1, Output: 7 + 4, CacheRead: 200}); u != want {
		t.Fatalf("second read = %+v, want %+v", u, want)
	}
	if fi, _ := os.Stat(path); tr.Offset != fi.Size() {
		t.Fatalf("offset %d, size %d", tr.Offset, fi.Size())
	}

	// A subagent's own transcript counts its sidechain lines.
	sub := filepath.Join(t.TempDir(), "agent-x1.jsonl")
	os.WriteFile(sub, src, 0o644)
	var st Transcript
	if u, _ := st.Read(sub); u.Input != 10+1000+1 {
		t.Fatalf("subagent read = %+v", u)
	}

	// A rewritten (shorter) file is read from the start.
	os.WriteFile(path, src[:cut], 0o644)
	tr.Offset = 1 << 20
	if u, _ := tr.Read(path); u.Input != 10 {
		t.Fatalf("after rewrite = %+v", u)
	}
	if _, err := tr.Read(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Fatal("missing file: no error")
	}
}

func TestSubagentTranscript(t *testing.T) {
	sess := filepath.Join("p", "S.jsonl")
	if got, want := SubagentTranscript(sess, "a1"), filepath.Join("p", "S", "subagents", "agent-a1.jsonl"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := SubagentTranscript(sess, ""); got != sess {
		t.Fatalf("lead: got %q", got)
	}
	own := filepath.Join("p", "S", "subagents", "agent-a1.jsonl")
	if got := SubagentTranscript(own, "a1"); got != own {
		t.Fatalf("own: got %q", got)
	}
}

func TestStartOffset(t *testing.T) {
	dir := t.TempDir()
	line := `{"type":"assistant","message":{"id":"msg_%s","usage":{"input_tokens":%d,"output_tokens":1}}}` + "\n"
	mk := func(id string, in int) string { return fmt.Sprintf(line, id, in) }

	// A complete main transcript starts at its size.
	main := filepath.Join(dir, "S.jsonl")
	old := mk("a", 1000) + mk("b", 2000)
	os.WriteFile(main, []byte(old), 0o644)
	if got := StartOffset(main); got != int64(len(old)) {
		t.Fatalf("main: %d, want %d", got, len(old))
	}

	// A half-written last line is left to be read later.
	part := mk("c", 4)
	os.WriteFile(main, []byte(old+part[:10]), 0o644)
	off := StartOffset(main)
	if off != int64(len(old)) {
		t.Fatalf("partial: %d, want %d", off, len(old))
	}
	// Reading from there counts only what follows the baseline.
	f, _ := os.OpenFile(main, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(part[10:] + mk("d", 8))
	f.Close()
	tr := Transcript{Offset: off}
	if u, err := tr.Read(main); err != nil || u.Input != 4+8 {
		t.Fatalf("read from start offset = %+v, %v", u, err)
	}

	// No newline at all: start at 0.
	nonl := filepath.Join(dir, "N.jsonl")
	os.WriteFile(nonl, []byte(part[:10]), 0o644)
	if got := StartOffset(nonl); got != 0 {
		t.Fatalf("no newline: %d", got)
	}

	// A subagent's own transcript counts from the start.
	sub := SubagentTranscript(main, "x1")
	os.MkdirAll(filepath.Dir(sub), 0o755)
	os.WriteFile(sub, []byte(old), 0o644)
	if got := StartOffset(sub); got != 0 {
		t.Fatalf("subagent: %d", got)
	}
	if !IsSubagentTranscript(sub) || IsSubagentTranscript(main) {
		t.Fatal("IsSubagentTranscript")
	}

	if got := StartOffset(filepath.Join(dir, "missing.jsonl")); got != 0 {
		t.Fatalf("missing: %d", got)
	}
}
