package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"tazzyronth/internal/hub"
	"tazzyronth/internal/proto"
)

// TestSharedShim runs a lead and two subagents through one shim, the way
// Claude Code subagents share their parent's MCP server.
func TestSharedShim(t *testing.T) {
	dir := t.TempDir()
	h, err := hub.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	c := newClient(t, func(context.Context) (*hub.Client, error) { return hub.Connect(dir) })
	c.request("initialize", map[string]any{"protocolVersion": "2025-06-18"})

	c.mustCall("tazzy_start", map[string]any{"goal": "g"})
	c.mustCall("tazzy_plan", map[string]any{"tasks": []map[string]any{
		{"id": "api", "files": []string{"src/api/**"}, "priority": 2},
		{"id": "handler", "files": []string{"src/api/h.go"}, "priority": 1},
	}})
	ra, _ := c.mustCall("tazzy_join", map[string]any{"name": "a"})
	rb, _ := c.mustCall("tazzy_join", map[string]any{"name": "b"})
	a, b := ra["agent"].(string), rb["agent"].(string)
	if a == b || ra["note"] == nil {
		t.Fatalf("join: %v / %v", ra, rb)
	}

	res, _ := c.mustCall("tazzy_next", map[string]any{"agent": a})
	if task, _ := res["task"].(map[string]any); task["id"] != "api" {
		t.Fatalf("a next: %v", res)
	}
	if res, _ := c.mustCall("tazzy_lease", map[string]any{"agent": a, "paths": []string{"src/api/**"}}); res["granted"] != true {
		t.Fatalf("a lease: %v", res)
	}
	res, _ = c.mustCall("tazzy_next", map[string]any{"agent": b})
	if task, _ := res["task"].(map[string]any); task["id"] != "handler" {
		t.Fatalf("b next: %v", res)
	}
	res, _ = c.mustCall("tazzy_lease", map[string]any{"agent": b, "paths": []string{`src\api\h.go`}})
	if held, _ := res["held_by"].([]any); res["granted"] != false || len(held) != 1 || held[0] != a {
		t.Fatalf("b conflicting lease: %v", res)
	}

	// b blocks on the lease while a finishes through the same shim; with
	// serialized tool calls a's done would queue behind b's wait.
	send := func(id int, name string, args any) {
		m, _ := proto.NewRequest(id, "tools/call", map[string]any{"name": name, "arguments": args})
		if err := c.w.Write(m); err != nil {
			t.Fatal(err)
		}
	}
	send(1001, "tazzy_wait", map[string]any{"agent": b, "until": "lease:src/api/h.go", "timeout": 5})
	send(1002, "tazzy_done", map[string]any{"agent": a, "task": "api", "summary": "ok"})
	got := map[string]map[string]any{}
	for range 2 {
		resp, err := c.r.Read()
		if err != nil {
			t.Fatal(err)
		}
		var r toolResult
		json.Unmarshal(resp.Result, &r)
		var v map[string]any
		json.Unmarshal([]byte(r.Content[0].Text), &v)
		got[string(resp.ID)] = v
	}
	if got["1001"]["met"] != true || got["1002"]["state"] != "done" {
		t.Fatalf("wait/done: %v", got)
	}
	if res, _ := c.mustCall("tazzy_lease", map[string]any{"agent": b, "paths": []string{"src/api/h.go"}}); res["granted"] != true {
		t.Fatalf("b lease after release: %v", res)
	}

	// Identities stay apart: b's question reaches the lead (the default
	// identity), not a.
	c.mustCall("tazzy_send", map[string]any{"agent": b, "to": "role:lead", "body": "q"})
	if _, inbox := c.mustCall("tazzy_status", map[string]any{"agent": a}); inbox != nil {
		t.Fatalf("a got the lead's inbox: %v", inbox)
	}
	_, inbox := c.mustCall("tazzy_status", map[string]any{})
	if msgs, _ := inbox["messages"].([]any); len(msgs) != 1 || msgs[0].(map[string]any)["from"] != b {
		t.Fatalf("lead inbox: %v", inbox)
	}
	// Only the owner can finish a task.
	if res, _, isErr := c.call("tazzy_done", map[string]any{"agent": a, "task": "handler", "summary": "x"}); !isErr {
		t.Fatalf("a finished b's task: %v", res)
	}
	res, _ = c.mustCall("tazzy_status", map[string]any{})
	if l, _ := res["leases"].([]any); len(l) != 1 {
		t.Fatalf("leases in status: %v", res["leases"])
	}
}
