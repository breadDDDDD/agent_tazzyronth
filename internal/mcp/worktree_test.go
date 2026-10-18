package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"tazzyronth/internal/hub"
)

// TestWorktreeTools: tazzy_start(isolation=worktree) reports the integration
// branch, tazzy_next hands a host worker its worktree, and tazzy_done merges.
func TestWorktreeTools(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	h, err := hub.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	connect := func(context.Context) (*hub.Client, error) { return hub.Connect(dir) }

	lead := newClient(t, connect)
	res, _ := lead.mustCall("tazzy_start", map[string]any{"goal": "g", "spawn": "host", "isolation": "worktree"})
	integ, _ := res["integration"].(string)
	if res["isolation"] != "worktree" || !strings.HasPrefix(integ, "tazzy/integration-") {
		t.Fatalf("tazzy_start = %v", res)
	}
	lead.mustCall("tazzy_plan", map[string]any{"tasks": []map[string]any{{"id": "t1", "files": []string{"x.txt"}}}})

	w := newClient(t, connect)
	j, _ := w.mustCall("tazzy_join", map[string]any{})
	me := j["agent"].(string)
	n, _ := w.mustCall("tazzy_next", map[string]any{"agent": me})
	wt, _ := n["worktree"].(string)
	if wt == "" || n["worktree_note"] == nil {
		t.Fatalf("tazzy_next = %v", n)
	}
	if err := os.WriteFile(filepath.Join(wt, "x.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.mustCall("tazzy_done", map[string]any{"agent": me, "task": "t1", "summary": "wrote x"})
	out, err := exec.Command("git", "-C", dir, "show", integ+":x.txt").Output()
	if err != nil || strings.TrimSpace(string(out)) != "hi" {
		t.Errorf("x.txt on %s = %q, %v", integ, out, err)
	}
	if s, _ := lead.mustCall("tazzy_status", map[string]any{}); s["integration"] != integ {
		t.Errorf("status integration = %v", s["integration"])
	}
}
