package sim

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"tazzyronth/internal/graph"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/lease"
	"tazzyronth/internal/store"
)

func TestSwarm(t *testing.T) {
	for _, seed := range []uint64{1, 7, 42} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cfg := Config{Agents: 10, Tasks: 50, Seed: seed, Files: 12, MaxDeps: 2, Work: 2 * time.Millisecond, FailRate: 0.05, MaxAttempts: 100}
		start := time.Now()
		rep, err := Run(ctx, t.TempDir(), cfg)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		t.Logf("seed %d: %s, %d claims, %d lease conflicts, %d failures", seed, time.Since(start).Round(time.Millisecond), rep.Claims, rep.LeaseConflicts, rep.Failures)
		for _, v := range rep.Violations {
			t.Errorf("seed %d: %s", seed, v)
		}
		if rep.Claims < cfg.Tasks || rep.LeaseConflicts == 0 {
			t.Errorf("seed %d: run too easy to mean anything: %+v", seed, rep)
		}
	}
}

// TestMonitor checks that the monitor flags what it is meant to catch.
func TestMonitor(t *testing.T) {
	m := &monitor{tasks: map[string]graph.Task{"a": {ID: "a"}, "b": {ID: "b", Deps: []string{"a"}}},
		owner: map[string]string{}, done: map[string]bool{}, held: map[string][]lease.Lease{}}
	ev := func(typ string, p any) store.Event {
		b, _ := json.Marshal(p)
		return store.Event{Type: typ, Data: b}
	}
	m.apply(ev("lease.acquire", hub.LeaseParams{Agent: "x", Paths: []string{"pkg/**"}}))
	m.apply(ev("lease.acquire", hub.LeaseParams{Agent: "y", Paths: []string{"pkg/a.go"}, Mode: lease.Read}))
	m.apply(ev("task.state", hub.TaskStateParams{ID: "b", State: graph.Claimed, Agent: "y"}))
	if len(m.violations) != 2 {
		t.Fatalf("violations = %q", m.violations)
	}
	m.apply(ev("lease.release", hub.LeaseReleaseParams{Agent: "x", Paths: []string{"pkg/**"}}))
	m.apply(ev("lease.acquire", hub.LeaseParams{Agent: "z", Paths: []string{"pkg/b.go"}}))
	if len(m.violations) != 2 {
		t.Fatalf("after release: %q", m.violations)
	}
	m.apply(ev("task.state", hub.TaskStateParams{ID: "a", State: graph.Claimed, Agent: "x"}))
	m.apply(ev("agent.leave", hub.LeaveParams{Agent: "x"}))
	m.apply(ev("task.state", hub.TaskStateParams{ID: "a", State: graph.Done, Agent: "x"}))
	if len(m.violations) != 3 {
		t.Fatalf("done by a removed owner: %q", m.violations)
	}
}

// TestChaos runs swarms whose agents crash, stall, go quiet, drop
// connections and fail tasks for good, with second-scale timeouts shrunk to
// milliseconds. Every task must still end done or failed with no overlapping
// write leases, no lost checkpoint and no task finished by a non-owner.
func TestChaos(t *testing.T) {
	var total Report
	for _, seed := range []uint64{1, 7, 42, 99} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cfg := Config{Agents: 10, Tasks: 50, Seed: seed, Files: 12, MaxDeps: 2, Work: 2 * time.Millisecond,
			FailRate: 0.05, KillRate: 0.04, StallRate: 0.04, DelayRate: 0.04, DropRate: 0.1, FatalRate: 0.01,
			Stall: 60 * time.Millisecond, MaxAttempts: 4}
		start := time.Now()
		rep, err := Run(ctx, t.TempDir(), cfg)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		t.Logf("seed %d: %s, %d done, %d failed; %d claims, %d kills, %d stalls (%d refused), %d delays, %d drops, %d failures",
			seed, time.Since(start).Round(time.Millisecond), rep.Done, rep.Failed, rep.Claims, rep.Kills, rep.Stalls, rep.Removed, rep.Delays, rep.Drops, rep.Failures)
		for _, v := range rep.Violations {
			t.Errorf("seed %d: %s", seed, v)
		}
		if rep.Done == 0 {
			t.Errorf("seed %d: nothing got done", seed)
		}
		total.Kills += rep.Kills
		total.Stalls += rep.Stalls
		total.Removed += rep.Removed
		total.Delays += rep.Delays
		total.Drops += rep.Drops
	}
	if total.Kills == 0 || total.Stalls == 0 || total.Removed == 0 || total.Delays == 0 || total.Drops == 0 {
		t.Errorf("some chaos never happened: %+v", total)
	}
}
