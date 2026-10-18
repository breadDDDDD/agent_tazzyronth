package mcp

import (
	"context"
	"strings"
	"testing"

	"tazzyronth/internal/hub"
)

// TestHandover: a task given up by one worker reaches the next with its
// checkpoints, the files touched and why the attempt ended; a removed worker
// is told to stop; the lead hears about a task that fails for good.
func TestHandover(t *testing.T) {
	dir := t.TempDir()
	h, err := hub.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	c := newClient(t, func(context.Context) (*hub.Client, error) { return hub.Connect(dir) })
	c.request("initialize", map[string]any{"protocolVersion": "2025-06-18"})

	c.mustCall("tazzy_start", map[string]any{"goal": "g"})
	c.mustCall("tazzy_plan", map[string]any{"tasks": []map[string]any{{"id": "x", "spec": "do x"}, {"id": "y", "deps": []string{"x"}}}})
	ra, _ := c.mustCall("tazzy_join", map[string]any{"name": "a"})
	rb, _ := c.mustCall("tazzy_join", map[string]any{"name": "b"})
	a, b := ra["agent"].(string), rb["agent"].(string)

	c.mustCall("tazzy_next", map[string]any{"agent": a})
	c.mustCall("tazzy_progress", map[string]any{"agent": a, "task": "x", "note": "parser done", "files_touched": []string{"x/parse.go"}})
	if res, _ := c.mustCall("tazzy_fail", map[string]any{"agent": a, "task": "x", "reason": "tests time out", "retryable": true}); res["state"] != "ready" {
		t.Fatalf("retryable fail: %v", res)
	}
	res, _ := c.mustCall("tazzy_next", map[string]any{"agent": b})
	task, _ := res["task"].(map[string]any)
	join := func(k string) string {
		var s []string
		for _, v := range task[k].([]any) {
			s = append(s, v.(string))
		}
		return strings.Join(s, "|")
	}
	if task["id"] != "x" || task["spec"] != "do x" || task["attempts"] != 2.0 || res["hint"] == nil ||
		join("checkpoints") != a+": parser done" || join("files_touched") != "x/parse.go" || join("failures") != a+": tests time out" {
		t.Fatalf("handover: %v", res)
	}

	// Remove b (as the stall monitor would); its late calls are refused.
	if _, _, isErr := c.call("tazzy_control", map[string]any{"action": "retire", "target": b}); isErr {
		t.Fatal("retire failed")
	}
	if res, _, isErr := c.call("tazzy_done", map[string]any{"agent": b, "task": "x", "summary": "late"}); !isErr || !strings.Contains(res["error"].(string), "was removed") {
		t.Fatalf("late done by removed b: %v", res)
	}

	// a takes it again and fails it for good: the lead is told, y fails too.
	c.mustCall("tazzy_next", map[string]any{"agent": a})
	if res, _ := c.mustCall("tazzy_fail", map[string]any{"agent": a, "task": "x", "reason": "spec is wrong"}); res["state"] != "failed" {
		t.Fatalf("fatal fail: %v", res)
	}
	_, inbox := c.mustCall("tazzy_status", map[string]any{})
	msgs, _ := inbox["messages"].([]any)
	if len(msgs) != 1 || !strings.Contains(msgs[0].(map[string]any)["body"].(string), "task x failed after 3 attempt") {
		t.Fatalf("lead inbox: %v", inbox)
	}
}
