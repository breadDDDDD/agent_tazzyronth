package hub

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/config"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/lease"
)

func TestWidth(t *testing.T) {
	s := newState(t.TempDir())
	s.join(JoinParams{ID: "w"}, time.Time{})
	if err := s.graph.Add(
		graph.Task{ID: "a", Files: []string{"src/**"}},
		graph.Task{ID: "b", Files: []string{"src/x.go"}}, // overlaps a
		graph.Task{ID: "c", Files: []string{"web/y.ts"}},
		graph.Task{ID: "d"},                               // no files: never conflicts
		graph.Task{ID: "e", Files: []string{"docs/z.md"}}, // leased below
		graph.Task{ID: "f", Deps: []string{"a"}},          // not ready
	); err != nil {
		t.Fatal(err)
	}
	s.leases.Acquire("w", []string{"docs"}, lease.Write, 60, time.Time{})
	if got := s.width(); got != 3 { // a, c, d
		t.Errorf("width = %d, want 3", got)
	}
}

func TestIdleShutdown(t *testing.T) {
	cfg := config.Default()
	cfg.Timeouts.HubIdle, cfg.Timeouts.ScaleTick = 0.2, 0.02
	dir := t.TempDir()
	h, err := StartWith(dir, Options{Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Idle():
		t.Fatal("idle with a connection open")
	case <-time.After(400 * time.Millisecond):
	}
	c.Close()
	select {
	case <-h.Idle():
	case <-time.After(3 * time.Second):
		t.Fatal("hub never went idle")
	}
}

func TestSpawnErrors(t *testing.T) {
	cfg := config.Default()
	cfg.Budget.MaxTotalSpawns = 1
	cfg.Adapters["broken"] = config.Adapter{Cmd: []string{"tazzy-no-such-binary"}}
	dir := t.TempDir()
	h, err := StartWith(dir, Options{Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if err := c.Call(ctx, "agent.spawn", SpawnParams{Adapter: "nope"}, nil); err == nil {
		t.Error("unknown adapter: no error")
	}
	// A failed start still counts against the budget and leaves no agent behind.
	if err := c.Call(ctx, "agent.spawn", SpawnParams{Adapter: "broken"}, nil); err == nil {
		t.Error("missing binary: no error")
	}
	if err := c.Call(ctx, "agent.spawn", SpawnParams{Adapter: "broken"}, nil); err == nil {
		t.Error("over budget: no error")
	}
	var agents []Agent
	if err := c.Call(ctx, "agent.list", nil, &agents); err != nil || len(agents) != 0 {
		t.Errorf("agents = %v, %v", agents, err)
	}
	if err := c.Call(ctx, "swarm.set", SwarmParams{Spawn: "bogus"}, nil); err == nil {
		t.Error("bad spawn mode: no error")
	}
}

func TestFinishReleasesWorkers(t *testing.T) {
	cfg := config.Default()
	cfg.Timeouts.ScaleTick, cfg.Timeouts.FinishGrace = 0.02, 0.1
	dir := t.TempDir()
	h, err := StartWith(dir, Options{Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call := func(method string, params, result any) {
		t.Helper()
		if err := c.Call(ctx, method, params, result); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	ids := func() (out []string) {
		var agents []Agent
		call("agent.list", nil, &agents)
		for _, a := range agents {
			out = append(out, a.ID)
		}
		return out
	}

	call("agent.join", JoinParams{ID: "lead", Role: "lead"}, nil)
	call("agent.join", JoinParams{ID: "w1"}, nil)
	call("agent.join", JoinParams{ID: "w2"}, nil)
	call("task.add", TaskAddParams{Tasks: []graph.Task{{ID: "x"}}}, nil)
	call("sched.next", AgentParams{Agent: "w1"}, nil)
	time.Sleep(300 * time.Millisecond)
	if got := ids(); len(got) != 3 {
		t.Fatalf("released while x is running: %v", got)
	}

	call("task.state", TaskStateParams{ID: "x", State: graph.Done, Agent: "w1"}, nil)
	deadline := time.Now().Add(3 * time.Second)
	for len(ids()) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("workers not released after the swarm finished: %v", ids())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := ids(); got[0] != "lead" {
		t.Fatalf("left = %v, want the lead", got)
	}
	err = c.Call(ctx, "sched.next", AgentParams{Agent: "w1"}, nil)
	if err == nil || !strings.Contains(err.Error(), "do not call tazzy_join") {
		t.Fatalf("released worker's call: %v", err)
	}
}

func TestPruneKeepsBusyWorkers(t *testing.T) {
	_, call := failHub(t, 300)
	call("agent.join", JoinParams{ID: "lead", Role: "lead"}, nil)
	call("agent.join", JoinParams{ID: "busy"}, nil)
	call("agent.join", JoinParams{ID: "leasing"}, nil)
	call("agent.join", JoinParams{ID: "idle"}, nil)
	call("task.add", TaskAddParams{Tasks: []graph.Task{{ID: "x"}}}, nil)
	call("sched.next", AgentParams{Agent: "busy"}, nil)
	call("lease.acquire", LeaseParams{Agent: "leasing", Paths: []string{"a.go"}}, nil)

	var res struct{ Released []string }
	call("agent.prune", nil, &res)
	if !slices.Equal(res.Released, []string{"idle"}) {
		t.Fatalf("released %v, want [idle]", res.Released)
	}
}
