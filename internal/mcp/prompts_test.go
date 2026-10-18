package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"tazzyronth/internal/hub"
	"tazzyronth/internal/proto"
	"tazzyronth/playbooks"
)

func TestPrompts(t *testing.T) {
	c := newClient(t, noHub)
	resp := c.request("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if !strings.Contains(string(resp.Result), `"prompts":{}`) {
		t.Errorf("prompts capability missing: %s", resp.Result)
	}

	var list struct{ Prompts []prompt }
	json.Unmarshal(c.request("prompts/list", nil).Result, &list)
	if len(list.Prompts) != 2 || list.Prompts[0].Name != "tazzy" || list.Prompts[0].Arguments[0].Name != "goal" || !list.Prompts[0].Arguments[0].Required {
		t.Fatalf("prompts/list = %+v", list.Prompts)
	}

	get := func(name string, args map[string]string) (string, *proto.Error) {
		resp := c.request("prompts/get", map[string]any{"name": name, "arguments": args})
		if resp.Error != nil {
			return "", resp.Error
		}
		var r struct {
			Messages []struct {
				Role    string
				Content struct{ Type, Text string }
			}
		}
		if err := json.Unmarshal(resp.Result, &r); err != nil || len(r.Messages) != 1 || r.Messages[0].Role != "user" || r.Messages[0].Content.Type != "text" {
			t.Fatalf("%s: bad result %s", name, resp.Result)
		}
		return r.Messages[0].Content.Text, nil
	}
	text, perr := get("tazzy", map[string]string{"goal": "Add OAuth login", "max": "3"})
	if perr != nil || !strings.HasPrefix(text, playbooks.Lead) || !strings.Contains(text, `tazzy_start(goal="Add OAuth login", max_agents=3)`) {
		t.Errorf("tazzy prompt = %q, %v", text, perr)
	}
	if text, perr := get("tazzy-worker", nil); perr != nil || text != playbooks.Worker {
		t.Errorf("worker prompt = %q, %v", text, perr)
	}
	for _, bad := range []struct {
		name string
		args map[string]string
	}{{"tazzy", nil}, {"tazzy", map[string]string{"goal": "g", "max": "lots"}}, {"nope", nil}} {
		if _, perr := get(bad.name, bad.args); perr == nil || perr.Code != proto.CodeInvalidParams {
			t.Errorf("%s %v: error = %v", bad.name, bad.args, perr)
		}
	}
}

// TestStartAuto: tazzy_start resolves spawn mode auto from the MCP client,
// returns the lead playbook, and opens the dashboard once per shim.
func TestStartAuto(t *testing.T) {
	dir := t.TempDir()
	h, err := hub.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	connect := func(context.Context) (*hub.Client, error) { return hub.Connect(dir) }

	start := func(clientName string) (*client, func() int) {
		srv := New(connect, "test", nil)
		opened := 0
		srv.SetLauncher(func() string { opened++; return "opened in a test pane" })
		c := serverClient(t, srv)
		c.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "clientInfo": map[string]string{"name": clientName}})
		return c, func() int { return opened }
	}

	c, opened := start("claude-code")
	res, _ := c.mustCall("tazzy_start", map[string]any{"goal": "g"})
	if res["spawn"] != "host" || res["playbook"] != playbooks.Lead || res["dashboard"] != "opened in a test pane" {
		t.Errorf("claude-code start = %v", res)
	}
	res, _ = c.mustCall("tazzy_start", map[string]any{"goal": "g"})
	if opened() != 1 || res["dashboard"] != nil {
		t.Errorf("dashboard opened %d times; second result %v", opened(), res)
	}
	w := newClient(t, connect)
	if res, _ := w.mustCall("tazzy_join", map[string]any{}); res["playbook"] != playbooks.Worker {
		t.Errorf("join result lacks the worker playbook: %v", res)
	}

	// ui "host" (the Claude Code mod drew its pane): no window is opened.
	c, opened = start("claude-code")
	if res, _ := c.mustCall("tazzy_start", map[string]any{"goal": "g", "ui": "host"}); opened() != 0 || res["dashboard"] == nil {
		t.Errorf("ui host: opened %d, result %v", opened(), res)
	}

	c, _ = start("codex-mcp-client")
	if res, _ := c.mustCall("tazzy_start", map[string]any{"goal": "g"}); res["spawn"] != "hub" || res["adapter"] != "codex" { // workers match the lead's host
		t.Errorf("codex start = %v", res)
	}
	if res, _ := c.mustCall("tazzy_start", map[string]any{"goal": "g", "spawn": "host"}); res["spawn"] != "host" {
		t.Errorf("explicit spawn ignored: %v", res)
	}
}
