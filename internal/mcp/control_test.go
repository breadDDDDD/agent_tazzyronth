package mcp

import (
	"context"
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
