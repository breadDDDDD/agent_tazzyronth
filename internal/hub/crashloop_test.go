package hub

import (
	"os"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/activity"
	"tazzyronth/internal/config"
	"tazzyronth/internal/graph"
)

// crashEnv makes the test binary, started as a hub-spawned worker, act as a
// worker CLI that lives for the given duration and then exits with status 3.
const crashEnv = "TAZZY_CRASH_HELPER"

func TestCrashHelper(t *testing.T) {
	life := os.Getenv(crashEnv)
	if life == "" {
		t.Skip("only runs as a fake worker")
	}
	d, _ := time.ParseDuration(life)
	time.Sleep(d)
	os.Exit(3)
}

// crashHub starts a hub that scales fast in hub mode with a fake worker
// adapter "crash" living for life before it exits 3.
func crashHub(t *testing.T, life string, quickExit float64, maxQuick int) (*Hub, func(method string, params, result any)) {
	t.Helper()
	cfg := config.Default()
	cfg.Timeouts.ScaleTick, cfg.Timeouts.SpawnCooldown = 0.02, 0
	cfg.Timeouts.QuickExit, cfg.Budget.MaxQuickExits = quickExit, maxQuick
	cfg.Adapters["crash"] = config.Adapter{
		Cmd: []string{os.Args[0], "-test.run=^TestCrashHelper$"},
		Env: map[string]string{crashEnv: life},
	}
	dir := t.TempDir()
	h, err := StartWith(dir, Options{Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	_, call := crashConn(t, dir)
	return h, call
}

// crashConn connects to the hub in dir with a call helper that fails
// the test on error.
func crashConn(t *testing.T, dir string) (*Client, func(method string, params, result any)) {
	t.Helper()
	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, func(method string, params, result any) {
		t.Helper()
		if err := c.Call(t.Context(), method, params, result); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
}

func (h *Hub) runState() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.st.runState()
}

func crashBlockers(t *testing.T, call func(string, any, any)) []string {
	t.Helper()
	var out []string
	for _, m := range leadInbox(t, call) {
		if m.From == hubSender && strings.HasPrefix(m.Body, "workers keep dying") {
			out = append(out, m.Body)
		}
	}
	return out
}

// waitFor polls cond until it holds, recording an alert for every live
// worker along the way (standing in for a CLI's quota error on its stream).
func (h *Hub) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	alerted := map[string]bool{}
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		for _, id := range h.sup.Live() {
			if !alerted[id] {
				alerted[id] = true
				h.record(activity.Activity{Agent: id, Kind: activity.Alert, Source: activity.SrcStream, Summary: "Individual quota reached"})
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCrashLoopPauses(t *testing.T) {
	h, call := crashHub(t, "300ms", 5, 3)
	call("agent.join", JoinParams{ID: "lead", Role: "lead"}, nil)
	call("task.add", TaskAddParams{Tasks: []graph.Task{{ID: "x"}}}, nil)
	call("swarm.set", SwarmParams{Spawn: config.SpawnHub, Adapter: "crash"}, nil)

	h.waitFor(t, "the crash guard's pause", func() bool { return h.runState() == RunPaused })
	if n := h.spawnedCount(); n != 3 {
		t.Fatalf("paused after %d spawns, want 3", n)
	}
	time.Sleep(300 * time.Millisecond)
	if n := h.spawnedCount(); n != 3 {
		t.Fatalf("spawned %d workers while paused, want 3", n)
	}
	got := crashBlockers(t, call)
	if len(got) != 1 || !strings.Contains(got[0], "(3 in a row; last: Individual quota reached): swarm paused") {
		t.Fatalf("lead messages = %q", got)
	}

	// Resume resets the count: three more quick deaths, not one, pause it again.
	call("swarm.control", ControlParams{Action: ActionResume, By: "lead"}, nil)
	h.waitFor(t, "the second pause", func() bool { return h.spawnedCount() >= 6 && h.runState() == RunPaused })
	if n := h.spawnedCount(); n != 6 {
		t.Fatalf("second pause after %d spawns, want 6", n)
	}
	if got := crashBlockers(t, call); len(got) != 2 {
		t.Fatalf("lead messages after the second pause = %q", got)
	}
}

// TestCrashLoopSurvivors: workers that outlive quick_exit_s aren't quick
// deaths, however many die afterwards.
func TestCrashLoopSurvivors(t *testing.T) {
	h, call := crashHub(t, "400ms", 0.1, 1)
	call("agent.join", JoinParams{ID: "lead", Role: "lead"}, nil)
	call("task.add", TaskAddParams{Tasks: []graph.Task{{ID: "x"}}}, nil)
	call("swarm.set", SwarmParams{Spawn: config.SpawnHub, Adapter: "crash"}, nil)
	h.waitFor(t, "three workers", func() bool { return h.spawnedCount() >= 3 })
	if s := h.runState(); s != RunRunning {
		t.Fatalf("state = %q, want running", s)
	}
	if got := crashBlockers(t, call); len(got) != 0 {
		t.Fatalf("lead told %q", got)
	}
}

func TestCrashLoopReset(t *testing.T) {
	cfg := config.Default()
	h := &Hub{cfg: cfg, st: newState(t.TempDir()), spawnedAt: map[string]time.Time{}}
	now := time.Now()

	h.quickDeaths = 2
	h.spawnedAt["young"] = now.Add(-time.Second)
	h.survivorsLocked(now)
	if h.quickDeaths != 2 || len(h.spawnedAt) != 1 {
		t.Fatalf("young worker: deaths %d, tracked %v", h.quickDeaths, h.spawnedAt)
	}
	h.survivorsLocked(now.Add(time.Minute))
	if h.quickDeaths != 0 || len(h.spawnedAt) != 0 {
		t.Fatalf("survivor: deaths %d, tracked %v", h.quickDeaths, h.spawnedAt)
	}

	// A newly done task resets the count too.
	if err := h.st.graph.Add(graph.Task{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	h.quickDeaths = 2
	h.survivorsLocked(now)
	if h.quickDeaths != 2 {
		t.Fatalf("nothing done: deaths %d", h.quickDeaths)
	}
	for _, s := range []graph.State{graph.Claimed, graph.Done} {
		if err := h.st.graph.SetState("a", s, "w"); err != nil {
			t.Fatal(err)
		}
	}
	h.survivorsLocked(now)
	if h.quickDeaths != 0 {
		t.Fatalf("task done: deaths %d", h.quickDeaths)
	}
}

func TestCrashLoopBackoff(t *testing.T) {
	cfg := config.Default()
	h := &Hub{cfg: cfg}
	for _, tc := range []struct {
		base float64
		dies int
		want time.Duration
	}{
		{5, 0, 5 * time.Second},
		{5, 1, 10 * time.Second},
		{5, 2, 20 * time.Second},
		{5, 3, 40 * time.Second},
		{5, 4, time.Minute},
		{5, 9, time.Minute},
		{0, 3, 0},
		{90, 2, 90 * time.Second},
	} {
		h.cfg.Timeouts.SpawnCooldown, h.quickDeaths = tc.base, tc.dies
		if got := h.spawnCooldownLocked(); got != tc.want {
			t.Errorf("cooldown %vs after %d quick deaths = %v, want %v", tc.base, tc.dies, got, tc.want)
		}
	}
}
