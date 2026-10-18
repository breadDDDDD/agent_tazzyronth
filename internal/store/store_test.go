package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "events.jsonl")
	l, evs, err := Open(path)
	if err != nil || len(evs) != 0 {
		t.Fatalf("Open = %v, %v", evs, err)
	}
	for i := range 3 {
		ev := Event{Time: time.Unix(int64(i), 0).UTC(), Type: "x", Data: json.RawMessage(`{"i":1}`)}
		if err := l.Append(&ev); err != nil {
			t.Fatal(err)
		}
		if ev.Seq != int64(i+1) {
			t.Fatalf("seq = %d, want %d", ev.Seq, i+1)
		}
	}
	l.Close()

	l, evs, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if len(evs) != 3 || evs[2].Seq != 3 || l.Seq() != 3 || string(evs[0].Data) != `{"i":1}` {
		t.Fatalf("reopened events = %+v, seq %d", evs, l.Seq())
	}
	ev := Event{Type: "y"}
	if err := l.Append(&ev); err != nil || ev.Seq != 4 {
		t.Fatalf("append after reopen: seq %d, %v", ev.Seq, err)
	}
}

func TestTornTailTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	good := `{"seq":1,"time":"2026-01-01T00:00:00Z","type":"a"}` + "\n"
	if err := os.WriteFile(path, []byte(good+`{"seq":2,"ti`), 0o600); err != nil {
		t.Fatal(err)
	}
	l, evs, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	ev := Event{Type: "b"}
	if err := l.Append(&ev); err != nil || ev.Seq != 2 {
		t.Fatalf("append: seq %d, %v", ev.Seq, err)
	}
	l.Close()
	evs, err = Read(path)
	if err != nil || len(evs) != 2 || evs[1].Type != "b" {
		t.Fatalf("Read = %+v, %v", evs, err)
	}
}

func TestCorruptMiddleIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data := "garbage\n" + `{"seq":1,"type":"a"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(path); err == nil {
		t.Fatal("Open succeeded on corrupt log")
	}
}
