package hub

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"tazzyronth/internal/graph"
	"tazzyronth/internal/lease"
	"tazzyronth/internal/proto"
)

func TestLeasesAndScheduling(t *testing.T) {
	dir := t.TempDir()
	startHub(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call := func(method string, params, result any) {
		t.Helper()
		if err := c.Call(ctx, method, params, result); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}

	next := func(agent string) string {
		t.Helper()
		var got *graph.Task
		call("sched.next", AgentParams{Agent: agent}, &got)
		if got == nil {
			return ""
		}
		if got.Owner != agent || got.State != graph.Claimed {
			t.Fatalf("sched.next(%s) = %+v", agent, got)
		}
		return got.ID
	}

	var w1, w2 Agent
	call("agent.join", JoinParams{}, &w1)
	call("agent.join", JoinParams{}, &w2)
	call("task.add", TaskAddParams{Tasks: []graph.Task{
		{ID: "api", Files: []string{`src\api\**`}},
		{ID: "ui", Files: []string{"web/**"}},
		{ID: "docs", Files: []string{"src/api/README.md"}, Priority: 1},
		{ID: "tests", Deps: []string{"api"}},
		{ID: "rust", Caps: []string{"rust"}, Priority: 9}, // nobody can
	}}, nil)

	// docs wins on priority over api's longer chain; rust is filtered out.
	if id := next(w2.ID); id != "docs" {
		t.Fatalf("w2 next = %s", id)
	}
	call("lease.acquire", LeaseParams{Agent: w2.ID, Paths: []string{"src/api/README.md"}}, nil)

	// w1 would pick api (critical path) but its files overlap w2's lease, so
	// the conflict penalty sends it to ui instead.
	if id := next(w1.ID); id != "ui" {
		t.Fatalf("w1 next = %s", id)
	}

	// A conflicting acquire fails with the holder named.
	err = c.Call(ctx, "lease.acquire", LeaseParams{Agent: w1.ID, Paths: []string{"web/**", "SRC/api/**"}}, nil)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != CodeConflict {
		t.Fatalf("conflicting acquire: %v", err)
	}
	var conflicts []lease.Conflict
	if json.Unmarshal(pe.Data, &conflicts) != nil || len(conflicts) != 1 || conflicts[0].Lease.Holder != w2.ID {
		t.Fatalf("conflict data = %s", pe.Data)
	}
	var check struct{ Conflicts []lease.Conflict }
	call("lease.check", LeaseCheckParams{Agent: w1.ID, Paths: []string{"src/api"}}, &check)
	if len(check.Conflicts) != 1 {
		t.Fatalf("lease.check = %+v", check)
	}

	// Only the owner may finish a task; finishing releases the owner's leases.
	if err := c.Call(ctx, "task.state", TaskStateParams{ID: "docs", State: graph.Done, Agent: w1.ID}, nil); err == nil {
		t.Fatal("w1 finished w2's task")
	}
	call("task.state", TaskStateParams{ID: "docs", State: graph.Done, Agent: w2.ID}, nil)
	call("lease.check", LeaseCheckParams{Agent: w1.ID, Paths: []string{"src/api"}}, &check)
	if len(check.Conflicts) != 0 {
		t.Fatalf("lease survived done: %+v", check)
	}

	// Leaving releases leases and requeues the agent's task.
	call("lease.acquire", LeaseParams{Agent: w1.ID, Paths: []string{"web/**"}}, nil)
	var left LeaveResult
	call("agent.leave", LeaveParams{Agent: w1.ID}, &left)
	if len(left.Released) != 1 || len(left.Tasks) != 1 || left.Tasks[0].Task.ID != "ui" || left.Tasks[0].Task.State != graph.Ready {
		t.Fatalf("leave = %+v", left)
	}
	var w3 Agent
	call("agent.join", JoinParams{}, &w3)
	if w3.ID == w1.ID {
		t.Fatalf("id %s reused after leave", w3.ID)
	}

	// api is free now and has the longer chain; then ui; rust never.
	for _, want := range []string{"api", "ui", ""} {
		if id := next(w3.ID); id != want {
			t.Fatalf("w3 next = %q, want %q", id, want)
		}
	}
}

func TestLeaseTTL(t *testing.T) {
	dir := t.TempDir()
	startHub(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	events, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var quiet, busy Agent
	for _, a := range []*Agent{&quiet, &busy} {
		if err := c.Call(ctx, "agent.join", JoinParams{}, a); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []Agent{quiet, busy} {
		if err := c.Call(ctx, "lease.acquire", LeaseParams{Agent: a.ID, Paths: []string{a.ID + "/**"}, TTL: 0.3}, nil); err != nil {
			t.Fatal(err)
		}
	}

	// busy keeps its lease alive with unrelated activity; quiet's lapses.
	var expired LeaseReleaseParams
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for expired.Agent == "" {
		select {
		case ev := <-events:
			if ev.Type == evRelease {
				json.Unmarshal(ev.Data, &expired)
			}
		case <-tick.C:
			c.Call(ctx, "msg.inbox", AgentParams{Agent: busy.ID}, nil)
		case <-deadline:
			t.Fatal("lease never expired")
		}
	}
	if expired.Agent != quiet.ID || expired.Reason != "expired" {
		t.Fatalf("released %+v", expired)
	}
	// Give busy's lease a few more expiry ticks to (wrongly) lapse.
	for range 6 {
		<-tick.C
		c.Call(ctx, "msg.inbox", AgentParams{Agent: busy.ID}, nil)
	}
	var held []lease.Lease
	if err := c.Call(ctx, "lease.list", nil, &held); err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].Holder != busy.ID {
		t.Fatalf("leases = %+v", held)
	}
}
