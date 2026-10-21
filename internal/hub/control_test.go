package hub

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/config"
	"tazzyronth/internal/graph"
)

// controlHub starts a hub in dir that scales fast with an adapter whose
// binary is missing, so every spawn attempt fails but still counts in
// state.spawned. It returns the hub and a call helper that fails the test on
// error.
func controlHub(t *testing.T, dir string) (*Hub, func(method string, params, result any)) {
	t.Helper()
	cfg := config.Default()
	cfg.Timeouts.ScaleTick, cfg.Timeouts.SpawnCooldown = 0.02, 0
	cfg.Adapters["broken"] = config.Adapter{Cmd: []string{"tazzy-no-such-binary"}}
	h, err := StartWith(dir, Options{Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return h, func(method string, params, result any) {
		t.Helper()
		if err := c.Call(ctx, method, params, result); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
}

func (h *Hub) spawnedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.st.spawned
}

func TestPauseResume(t *testing.T) {
	h, call := controlHub(t, t.TempDir())
	call("agent.join", JoinParams{ID: "lead", Role: "lead"}, nil)
	call("agent.join", JoinParams{ID: "w"}, nil)
	call("task.add", TaskAddParams{Tasks: []graph.Task{{ID: "x"}, {ID: "y"}}}, nil)

	var res ControlResult
	call("swarm.control", ControlParams{Action: ActionPause, By: "lead"}, &res)
	if res.State != RunPaused {
		t.Fatalf("pause: state = %q", res.State)
	}
	var got *graph.Task
	call("sched.next", AgentParams{Agent: "w"}, &got)
	if got != nil {
		t.Fatalf("claimed %s while paused", got.ID)
	}
	// The autoscaler spawns nothing while paused, though ready work is waiting.
	call("swarm.set", SwarmParams{Spawn: config.SpawnHub, Adapter: "broken"}, nil)
	time.Sleep(200 * time.Millisecond)
	if n := h.spawnedCount(); n != 0 {
		t.Fatalf("spawned %d workers while paused", n)
	}
	var snap Snapshot
	call("status", nil, &snap)
	if snap.State != RunPaused {
		t.Fatalf("status state = %q, want paused", snap.State)
	}
	var msgs []struct{ Kind, Body string }
	call("msg.inbox", AgentParams{Agent: "w"}, &msgs)
	if !slices.ContainsFunc(msgs, func(m struct{ Kind, Body string }) bool { return strings.HasPrefix(m.Body, "swarm paused") }) {
		t.Fatalf("no pause broadcast: %+v", msgs)
	}

	call("swarm.control", ControlParams{Action: ActionResume}, &res)
	if res.State != RunRunning {
		t.Fatalf("resume: state = %q", res.State)
	}
	call("sched.next", AgentParams{Agent: "w"}, &got)
	if got == nil || got.ID != "x" {
		t.Fatalf("after resume claimed %+v, want x", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.spawnedCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("autoscaler never spawned after resume")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStop(t *testing.T) {
	dir := t.TempDir()
	h, call := controlHub(t, dir)
	call("agent.join", JoinParams{ID: "lead", Role: "lead"}, nil)
	call("agent.join", JoinParams{ID: "w"}, nil)
	call("agent.join", JoinParams{ID: "w2"}, nil)
	call("task.add", TaskAddParams{Tasks: []graph.Task{
		{ID: "a"},
		{ID: "b", Deps: []string{"a"}},
		{ID: "c", Deps: []string{"b"}},
		{ID: "d"},
		{ID: "e"},
	}}, nil)
	var got, done *graph.Task
	call("sched.next", AgentParams{Agent: "w"}, &got) // stays claimed: cancelled by the stop
	call("lease.acquire", LeaseParams{Agent: "w", Paths: []string{"a.go"}}, nil)
	call("sched.next", AgentParams{Agent: "w2"}, &done)
	if got == nil || done == nil {
		t.Fatalf("claims: %+v, %+v", got, done)
	}
	call("task.state", TaskStateParams{ID: done.ID, State: graph.Done, Agent: "w2"}, nil)
	var cancelled []string
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if id != done.ID {
			cancelled = append(cancelled, id)
		}
	}

	var res ControlResult
	call("swarm.control", ControlParams{Action: ActionStop, By: "lead"}, &res)
	if res.State != RunStopped {
		t.Fatalf("stop: state = %q", res.State)
	}
	var snap Snapshot
	call("status", nil, &snap)
	for _, task := range snap.Tasks {
		want := graph.Cancelled
		if task.ID == done.ID {
			want = graph.Done
		}
		if task.State != want {
			t.Errorf("task %s is %s, want %s", task.ID, task.State, want)
		}
	}
	if len(snap.Leases) != 0 {
		t.Errorf("leases left after stop: %+v", snap.Leases)
	}
	var msgs []struct{ From, Kind, Body string }
	call("msg.inbox", AgentParams{Agent: "w"}, &msgs)
	if !slices.ContainsFunc(msgs, func(m struct{ From, Kind, Body string }) bool {
		return m.Kind == "directive" && m.Body == "swarm stopped by lead: stop work now, release leases, exit"
	}) {
		t.Errorf("no stop directive: %+v", msgs)
	}
	call("msg.inbox", AgentParams{Agent: "lead"}, &msgs)
	if !slices.ContainsFunc(msgs, func(m struct{ From, Kind, Body string }) bool {
		return m.Kind == "blocker" && m.Body == "swarm stopped by lead; cancelled: "+strings.Join(cancelled, ", ")
	}) {
		t.Errorf("lead not notified: %+v", msgs)
	}

	for _, a := range []string{ActionResume, ActionPause, ActionStop} {
		if err := h.callErr("swarm.control", ControlParams{Action: a}); err == nil {
			t.Errorf("%s after stop: no error", a)
		}
	}
	call("task.add", TaskAddParams{Tasks: []graph.Task{{ID: "late"}}}, nil)
	call("sched.next", AgentParams{Agent: "w"}, &got)
	if got != nil {
		t.Fatalf("claimed %s after stop", got.ID)
	}

	// The run state survives a restart.
	call("status", nil, &snap)
	h.Close()
	_, call = controlHub(t, dir)
	var after Snapshot
	call("status", nil, &after)
	if mustJSON(t, snap) != mustJSON(t, after) || after.State != RunStopped {
		t.Fatalf("state changed across restart\nbefore: %s\nafter:  %s", mustJSON(t, snap), mustJSON(t, after))
	}

	// A new swarm (tazzy_start's swarm.set) runs again.
	call("swarm.set", SwarmParams{}, nil)
	call("sched.next", AgentParams{Agent: "w"}, &got)
	if got == nil || got.ID != "late" {
		t.Fatalf("new swarm claimed %+v, want late", got)
	}
}

func TestControlErrors(t *testing.T) {
	h, _ := controlHub(t, t.TempDir())
	if err := h.callErr("swarm.control", ControlParams{Action: "halt"}); err == nil {
		t.Error("unknown action: no error")
	}
}

// callErr makes one call on a fresh connection and returns its error.
func (h *Hub) callErr(method string, params any) error {
	c, err := Connect(h.dir)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Call(context.Background(), method, params, nil)
}
