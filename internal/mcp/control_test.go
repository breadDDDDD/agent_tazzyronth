package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"tazzyronth/internal/hub"
)

// TestControl covers tazzy_start's spawn settings, tazzy_control
// spawn/retire, and a hub-spawned worker's shim keeping its preset id.
func TestControl(t *testing.T) {
	dir := t.TempDir()
	h, err := hub.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	connect := func(context.Context) (*hub.Client, error) { return hub.Connect(dir) }
	hc, err := hub.Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()

	lead := newClient(t, connect)
	lead.mustCall("tazzy_start", map[string]any{"goal": "g", "spawn": "host", "max_agents": 2, "adapter": "codex"})
	var snap hub.Snapshot
	if err := hc.Call(context.Background(), "status", nil, &snap); err != nil || snap.Swarm != (hub.SwarmParams{Spawn: "host", MaxAgents: 2, Adapter: "codex"}) {
		t.Fatalf("swarm = %+v, %v", snap.Swarm, err)
	}
	if res, _, isErr := lead.call("tazzy_start", map[string]any{"goal": "g", "spawn": "nope"}); !isErr {
		t.Errorf("bad spawn mode accepted: %v", res)
	}
	if res, _, isErr := lead.call("tazzy_control", map[string]any{"action": "spawn", "target": "no-such-adapter"}); !isErr || !strings.Contains(res["error"].(string), "unknown adapter") {
		t.Errorf("spawn unknown adapter: %v", res)
	}

	// What the supervisor does: pre-register, then start a shim with -agent.
	if err := hc.Call(context.Background(), "agent.join", hub.JoinParams{ID: "a5", Role: "worker", Adapter: "fake"}, nil); err != nil {
		t.Fatal(err)
	}
	srv := New(connect, "test", nil)
	srv.SetAgent("a5")
	w := serverClient(t, srv)
	if res, _ := w.mustCall("tazzy_join", map[string]any{"name": "w", "caps": []string{"go"}}); res["agent"] != "a5" {
		t.Fatalf("join with preset id: %v", res)
	}
	var agents []hub.Agent
	hc.Call(context.Background(), "agent.list", nil, &agents)
	if len(agents) != 2 || agents[0].ID != "a5" || agents[0].Adapter != "fake" || agents[0].Caps[0] != "go" {
		t.Errorf("agents = %+v", agents)
	}

	if res, _ := lead.mustCall("tazzy_control", map[string]any{"action": "retire", "target": "a5"}); res["retired"] != "a5" {
		t.Errorf("retire: %v", res)
	}
	if res, _, isErr := w.call("tazzy_next", map[string]any{}); !isErr {
		t.Errorf("retired worker got next: %v", res)
	}
}

// TestPauseStop covers tazzy_control pause/resume/stop and what tazzy_next
// and tazzy_status say meanwhile.
func TestPauseStop(t *testing.T) {
	dir := t.TempDir()
	h, err := hub.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	connect := func(context.Context) (*hub.Client, error) { return hub.Connect(dir) }

	lead := newClient(t, connect)
	lead.mustCall("tazzy_start", map[string]any{"goal": "g", "spawn": "host"})
	lead.mustCall("tazzy_plan", map[string]any{"tasks": []map[string]any{{"id": "x"}, {"id": "y"}}})
	w := newClient(t, connect)
	w.mustCall("tazzy_join", map[string]any{})

	if res, _ := lead.mustCall("tazzy_control", map[string]any{"action": "pause"}); res["state"] != hub.RunPaused {
		t.Fatalf("pause: %v", res)
	}
	if res, _ := w.mustCall("tazzy_next", map[string]any{"wait": 1}); res["task"] != nil || res["state"] != hub.RunPaused || !strings.Contains(res["note"].(string), "paused") {
		t.Fatalf("next while paused: %v", res)
	}
	if res, _ := lead.mustCall("tazzy_status", map[string]any{}); res["state"] != hub.RunPaused {
		t.Errorf("status while paused: %v", res)
	}
	if res, _ := lead.mustCall("tazzy_control", map[string]any{"action": "resume"}); res["state"] != hub.RunRunning {
		t.Fatalf("resume: %v", res)
	}
	if res, _ := lead.mustCall("tazzy_status", map[string]any{}); res["state"] != nil {
		t.Errorf("status while running: %v", res)
	}
	if res, _ := w.mustCall("tazzy_next", map[string]any{}); res["task"] == nil {
		t.Fatalf("next after resume: %v", res)
	}

	if res, _ := lead.mustCall("tazzy_control", map[string]any{"action": "stop"}); res["state"] != hub.RunStopped {
		t.Fatalf("stop: %v", res)
	}
	res, inbox := w.mustCall("tazzy_next", map[string]any{})
	if res["task"] != nil || res["state"] != hub.RunStopped || !strings.Contains(res["note"].(string), "exit") {
		t.Fatalf("next after stop: %v", res)
	}
	if !strings.Contains(mustJSONString(t, inbox), "swarm stopped by lead") {
		t.Errorf("no stop directive in inbox: %v", inbox)
	}
	if res, _, isErr := lead.call("tazzy_control", map[string]any{"action": "resume"}); !isErr {
		t.Errorf("resume after stop: %v", res)
	}
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
