package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/activity"
	"tazzyronth/internal/hub"
)

func TestHookReport(t *testing.T) {
	dir := t.TempDir()
	payload := `{"session_id":"S","agent_id":"sub-9","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"make"},"cwd":"/elsewhere"}`

	// No hub: still exit 0, quickly. Garbage and bad flags too.
	start := time.Now()
	for _, in := range []string{payload, "not json", ""} {
		if code := hookReport([]string{"-dir", dir}, strings.NewReader(in), io.Discard); code != 0 {
			t.Errorf("exit %d with no hub", code)
		}
	}
	if code := hookReport([]string{"-bogus"}, strings.NewReader(payload), io.Discard); code != 0 {
		t.Errorf("exit %d on a bad flag", code)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %s with no hub", d)
	}

	h, err := hub.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c, err := hub.Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if err := c.Call(ctx, "agent.join", hub.JoinParams{ID: "w7"}, nil); err != nil {
		t.Fatal(err)
	}
	// -agent (TAZZY_AGENT_ID) binds the host identity to w7.
	if code := hookReport([]string{"-dir", dir, "-agent", "w7"}, strings.NewReader(payload), io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var as []activity.Activity
	if err := c.Call(ctx, "activity.list", hub.ActivityListParams{Agent: "w7"}, &as); err != nil {
		t.Fatal(err)
	}
	if len(as) != 1 || as[0].Kind != activity.Cmd || as[0].Summary != "Bash make" || as[0].Source != activity.SrcHook {
		t.Errorf("activity = %+v", as)
	}
}

func TestHookReportEnforce(t *testing.T) {
	dir := t.TempDir()
	edit := func(agentID, path string) string {
		b, _ := json.Marshal(map[string]any{
			"session_id": "S", "agent_id": agentID, "hook_event_name": "PreToolUse",
			"tool_name": "Edit", "tool_input": map[string]string{"file_path": path},
		})
		var out bytes.Buffer
		if code := hookReport([]string{"-dir", dir, "-enforce"}, bytes.NewReader(b), &out); code != 0 {
			t.Fatalf("exit %d", code)
		}
		return out.String()
	}
	// No hub: allowed, nothing printed.
	if out := edit("sub-1", filepath.Join(dir, "a.go")); out != "" {
		t.Fatalf("no hub: %q", out)
	}

	h, err := hub.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c, err := hub.Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	for _, id := range []string{"w1", "w2"} {
		if err := c.Call(ctx, "agent.join", hub.JoinParams{ID: id}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Call(ctx, "lease.acquire", hub.LeaseParams{Agent: "w1", Paths: []string{"a.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	// Bind host ids to agents the way tazzy_join's call does (tool_input.agent).
	for host, id := range map[string]string{"sub-1": "w1", "sub-2": "w2"} {
		b, _ := json.Marshal(map[string]any{"session_id": "S", "agent_id": host, "hook_event_name": "PreToolUse",
			"tool_name": "mcp__plugin_tazzyronth_tazzyronth__tazzy_next", "tool_input": map[string]string{"agent": id}})
		hookReport([]string{"-dir", dir}, bytes.NewReader(b), io.Discard)
	}

	if out := edit("sub-1", filepath.Join(dir, "a.go")); out != "" {
		t.Errorf("holder denied: %q", out)
	}
	out := edit("sub-2", filepath.Join(dir, "a.go"))
	var dec struct {
		H struct{ PermissionDecision, PermissionDecisionReason string } `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(out), &dec) != nil || dec.H.PermissionDecision != "deny" || !strings.Contains(dec.H.PermissionDecisionReason, "w1") {
		t.Errorf("other agent: %q", out)
	}
	if out := edit("sub-2", filepath.Join(dir, "b.go")); out != "" {
		t.Errorf("unleased file denied: %q", out)
	}
	if out := edit("sub-3", filepath.Join(t.TempDir(), "outside.go")); out != "" {
		t.Errorf("file outside the project denied: %q", out)
	}
}
