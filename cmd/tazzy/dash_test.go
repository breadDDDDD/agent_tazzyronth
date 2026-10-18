package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"tazzyronth/internal/graph"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/store"
)

// TestWatch: no hub → one "disconnected"; then "status" first, live events
// (logged and activity), a fresh "status" after a change, and "disconnected"
// again when the hub goes away.
func TestWatch(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var got []store.Event
	types := func() []string {
		mu.Lock()
		defer mu.Unlock()
		var ts []string
		for _, e := range got {
			ts = append(ts, e.Type)
		}
		return ts
	}
	waitFor := func(what string, f func([]string) bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !f(types()) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s; got %v", what, types())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	count := func(ts []string, typ string) int {
		n := 0
		for _, x := range ts {
			if x == typ {
				n++
			}
		}
		return n
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watch(ctx, dir, func(ev store.Event) error {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
			return nil
		})
	}()
	waitFor("disconnected", func(ts []string) bool { return count(ts, "disconnected") == 1 })

	h, err := hub.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	waitFor("status", func(ts []string) bool { return count(ts, "status") == 1 })
	c, err := hub.Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	cx := context.Background()
	c.Call(cx, "agent.join", hub.JoinParams{ID: "w1"}, nil)
	c.Call(cx, "task.add", hub.TaskAddParams{Tasks: []graph.Task{{ID: "t1"}}}, nil)
	c.Call(cx, "task.state", hub.TaskStateParams{ID: "t1", State: graph.Claimed, Agent: "w1"}, nil)
	c.Close()
	waitFor("events", func(ts []string) bool {
		return count(ts, "task.add") == 1 && count(ts, "activity") >= 1 && count(ts, "status") >= 2
	})
	mu.Lock()
	var snap hub.Snapshot
	last := got[len(got)-1]
	for _, e := range got {
		if e.Type == "status" {
			json.Unmarshal(e.Data, &snap)
		}
	}
	mu.Unlock()
	if len(snap.Tasks) != 1 || snap.Tasks[0].State != graph.Claimed {
		t.Errorf("last status = %+v (last event %s)", snap.Tasks, last.Type)
	}
	h.Close()
	waitFor("second disconnected", func(ts []string) bool { return count(ts, "disconnected") == 2 })
	cancel()
	if err := <-done; err != nil {
		t.Errorf("watch: %v", err)
	}
}

func TestStatusCommand(t *testing.T) {
	dir := t.TempDir()
	if code := runStatus([]string{"-dir", dir}); code != 1 {
		t.Errorf("status with no hub = %d", code)
	}
	if code := runStatus([]string{"-dir", dir, "--line"}); code != 0 {
		t.Errorf("status --line with no hub = %d", code)
	}
	h, err := hub.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if code := runStatus([]string{"-dir", dir}); code != 0 {
		t.Errorf("status = %d", code)
	}
	if code := runTUI([]string{"-dir", dir, "-frame", "80x24"}); code != 0 {
		t.Errorf("tui -frame = %d", code)
	}
}
