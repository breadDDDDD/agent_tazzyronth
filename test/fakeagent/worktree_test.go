package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/graph"
	"tazzyronth/internal/hub"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestWorktreeWorkers: two hub-spawned workers edit different files in
// parallel, each in its own worktree (the fake edits files in its cwd); both
// changes end up on the integration branch, the user's branch and checkout
// are untouched, and the worktrees are gone once the workers retire.
func TestWorktreeWorkers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.name", "test")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "core.autocrlf", "false")
	for _, f := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "init")
	head := git(t, dir, "rev-parse", "main")

	_, _, c := startHubIn(t, dir, map[string]any{"isolation": "worktree"}, 2, "-work", "300ms", "-claude-stream")
	tasks := []graph.Task{{ID: "a", Files: []string{"a.go"}}, {ID: "b", Files: []string{"b.go"}}}
	if err := c.Call(context.Background(), "task.add", hub.TaskAddParams{Tasks: tasks}, nil); err != nil {
		t.Fatal(err)
	}
	var s hub.Snapshot
	poll(t, 30*time.Second, "all tasks done", func() bool { s = status(t, c); return allDone(s) })
	poll(t, 10*time.Second, "workers retired", func() bool { return spawnedLive(status(t, c)) == 0 })
	trees := filepath.Join(hub.StateDir(dir), "worktrees")
	poll(t, 10*time.Second, "worktrees removed", func() bool {
		ents, _ := os.ReadDir(trees)
		return len(ents) == 1 && ents[0].Name() == ".gitignore"
	})

	integ := s.Swarm.Integration
	if integ == "" {
		t.Fatal("no integration branch")
	}
	for _, f := range []string{"a.go", "b.go"} {
		got := git(t, dir, "show", integ+":"+f)
		if !strings.Contains(got, "// edited by a") {
			t.Errorf("%s on %s = %q", f, integ, got)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, f)); string(b) != "package x\n" {
			t.Errorf("checkout's %s changed: %q", f, b)
		}
	}
	if git(t, dir, "rev-parse", "main") != head || git(t, dir, "symbolic-ref", "--short", "HEAD") != "main" {
		t.Error("the user's branch moved")
	}
	if wl := git(t, dir, "worktree", "list", "--porcelain"); strings.Count(wl, "worktree ") != 1 {
		t.Errorf("git worktree list:\n%s", wl)
	}
	maxLive, left := workerLog(t, dir)
	if maxLive != 2 {
		t.Errorf("max live workers = %d, want 2 (in parallel)", maxLive)
	}
	t.Logf("integration %s, workers %v", integ, left)
}
