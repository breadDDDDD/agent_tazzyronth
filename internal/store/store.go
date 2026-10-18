// Package store is the append-only event log (.tazzyronth/events.jsonl) that
// is the hub's source of truth. State is rebuilt by replaying it in order.
package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Event is one state change. Data is the event-type-specific payload; applying
// the same events in the same order must always yield the same state.
type Event struct {
	Seq  int64           `json:"seq"`
	Time time.Time       `json:"time"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Log appends events to a JSONL file. It is not safe for concurrent use; the
// hub serialises access with its state mutex.
type Log struct {
	f   *os.File
	seq int64
}

// Open opens (creating if needed) the log at path and returns it together with
// the events already in it. A torn final line, left by a crash mid-write, is
// discarded and truncated away; any other malformed line is an error.
func Open(path string) (*Log, []Event, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, err
	}
	events, good, err := parse(f)
	if err == nil {
		err = f.Truncate(good)
	}
	if err == nil {
		_, err = f.Seek(good, 0)
	}
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("store: %s: %w", path, err)
	}
	l := &Log{f: f}
	if n := len(events); n > 0 {
		l.seq = events[n-1].Seq
	}
	return l, events, nil
}

// Read returns every event in the log at path without opening it for writing.
func Read(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	events, _, err := parse(f)
	return events, err
}

// parse reads all events and returns the offset just past the last good line.
func parse(f *os.File) ([]Event, int64, error) {
	data, err := readAll(f)
	if err != nil {
		return nil, 0, err
	}
	var events []Event
	var off int64
	for len(data) > 0 {
		line, rest, complete := bytes.Cut(data, []byte{'\n'})
		if !complete {
			// Each event is written with its newline in one Write, so a
			// newline-less tail is a torn write: drop it.
			break
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, 0, fmt.Errorf("event log line after seq %d: %w", lastSeq(events), err)
		}
		events = append(events, ev)
		off += int64(len(line)) + 1
		data = rest
	}
	return events, off, nil
}

func readAll(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	_, err := buf.ReadFrom(f)
	return buf.Bytes(), err
}

func lastSeq(events []Event) int64 {
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Seq
}

// Append assigns ev the next sequence number and writes it as one line.
func (l *Log) Append(ev *Event) error {
	ev.Seq = l.seq + 1
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return err
	}
	l.seq = ev.Seq
	return nil
}

// Seq is the sequence number of the last appended event.
func (l *Log) Seq() int64 { return l.seq }

func (l *Log) Close() error { return l.f.Close() }
