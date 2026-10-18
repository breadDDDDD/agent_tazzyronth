package hub

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/activity"
	"tazzyronth/internal/config"
	"tazzyronth/internal/graph"
)

// gitRepo makes a throwaway repo on branch main with one commit of files.
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.name", "test")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "core.autocrlf", "false")
	for name, content := range files {
		writeFile(t, dir, name, content)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func wtHub(t *testing.T, dir string, cfg config.Config) (call func(string, any, any), callErr func(string, any) error) {
	t.Helper()
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	callErr = func(method string, params any) error { return c.Call(ctx, method, params, nil) }
	return func(method string, params, result any) {
		t.Helper()
		if err := c.Call(ctx, method, params, result); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}, callErr
}

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

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestWorktreeIsolation drives host-style workers through worktree
// isolation: parallel edits of different files merge cleanly, edits of the
// same lines produce a conflict task that blocks the dependents until it is
// resolved, an unfinished attempt keeps its branch, worktrees go with their
// agents, and the user's branch is never touched.
func TestWorktreeIsolation(t *testing.T) {
	dir := gitRepo(t, map[string]string{"a.txt": "one\ntwo\nthree\n", "b.txt": "x\n"})
	mainHead := git(t, dir, "rev-parse", "main")

	cfg := config.Default()
	cfg.Isolation = config.IsolationWorktree
	cfg.Activity.Poll, cfg.Activity.Sweep = 0.05, 0
	call, callErr := wtHub(t, dir, cfg)

	var sw SwarmParams
	call("swarm.set", SwarmParams{Spawn: config.SpawnHost}, &sw)
	integ := sw.Integration
	if sw.Isolation != config.IsolationWorktree || !strings.HasPrefix(integ, "tazzy/integration-") {
		t.Fatalf("swarm.set = %+v", sw)
	}
	for _, id := range []string{"w1", "w2", "w3"} {
		call("agent.join", JoinParams{ID: id}, nil)
	}
	next := func(agent, want string) TaskStateResult {
		t.Helper()
		var r *TaskStateResult
		call("sched.next", AgentParams{Agent: agent}, &r)
		if r == nil || r.ID != want || r.Worktree == "" {
			t.Fatalf("%s: sched.next = %+v, want %s with a worktree", agent, r, want)
		}
		return *r
	}
	done := func(agent, task string) error {
		return callErr("task.state", TaskStateParams{ID: task, State: graph.Done, Agent: agent, Result: &graph.Result{Summary: task + " done"}})
	}
	show := func(path string) string { return git(t, dir, "show", integ+":"+path) }

	// Different files in parallel: both merge cleanly.
	call("task.add", TaskAddParams{Tasks: []graph.Task{
		{ID: "ta", Files: []string{"a.txt"}}, {ID: "tb", Files: []string{"b.txt"}},
		{ID: "tc", Deps: []string{"ta", "tb"}},
	}}, nil)
	ra, rb := next("w1", "ta"), next("w2", "tb")
	if ra.Worktree != filepath.Join(dir, ".tazzyronth", "worktrees", "w1") {
		t.Errorf("worktree = %s", ra.Worktree)
	}
	var lr map[string][]struct{ Path string }
	call("lease.acquire", LeaseParams{Agent: "w1", Paths: []string{filepath.Join(ra.Worktree, "a.txt")}}, &lr)
	if len(lr["leases"]) != 1 || lr["leases"][0].Path != "a.txt" {
		t.Errorf("lease on an absolute worktree path = %+v", lr)
	}
	writeFile(t, ra.Worktree, "a.txt", "ONE\ntwo\nthree\n")
	writeFile(t, rb.Worktree, "b.txt", "x\ny\n")
	writeFile(t, rb.Worktree, "new.txt", "new\n")
	e := waitActivity(t, call, "w1", func(a activity.Activity) bool { return a.Kind == activity.Edit && a.Source == activity.SrcDiff })
	if !strings.Contains(string(e.Detail), "+ONE") {
		t.Errorf("live diff from the worktree = %s", e.Detail)
	}
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "one\ntwo\nthree\n" {
		t.Errorf("project checkout changed: %q", got)
	}
	if err := done("w1", "ta"); err != nil {
		t.Fatal(err)
	}
	if err := done("w2", "tb"); err != nil {
		t.Fatal(err)
	}
	if show("a.txt") != "ONE\ntwo\nthree" || show("b.txt") != "x\ny" || show("new.txt") != "new" {
		t.Errorf("integration: a=%q b=%q", show("a.txt"), show("b.txt"))
	}
	// The dependent starts from the merged work; finishing it with no changes is fine.
	rc := next("w1", "tc")
	if got := readFile(t, filepath.Join(rc.Worktree, "b.txt")); got != "x\ny\n" {
		t.Errorf("dependent's worktree b.txt = %q", got)
	}
	if err := done("w1", "tc"); err != nil {
		t.Fatal(err)
	}

	// The same lines in parallel: the second merge conflicts.
	call("task.add", TaskAddParams{Tasks: []graph.Task{
		{ID: "td", Files: []string{"a.txt"}}, {ID: "te", Files: []string{"a.txt"}},
		{ID: "tf", Deps: []string{"te"}},
	}}, nil)
	re, rd := next("w1", "te"), next("w2", "td") // te first: it is on the critical path
	writeFile(t, rd.Worktree, "a.txt", "ONE\ntwo by d\nthree\n")
	writeFile(t, re.Worktree, "a.txt", "ONE\ntwo by e\nthree\n")
	if err := done("w2", "td"); err != nil {
		t.Fatal(err)
	}
	if err := done("w1", "te"); err != nil {
		t.Fatal(err)
	}
	var m, f graph.Task
	call("task.get", IDParams{ID: "merge-te"}, &m)
	call("task.get", IDParams{ID: "tf"}, &f)
	if m.State != graph.Ready || m.Merge != "tazzy/te" || len(m.Files) != 1 || m.Files[0] != "a.txt" || f.State != graph.Pending {
		t.Fatalf("conflict task = %+v; dependent = %s", m, f.State)
	}
	if show("a.txt") != "ONE\ntwo by d\nthree" {
		t.Errorf("integration after conflict = %q", show("a.txt"))
	}
	rm := next("w3", "merge-te")
	conflicted := readFile(t, filepath.Join(rm.Worktree, "a.txt"))
	if !strings.Contains(conflicted, "<<<<<<<") || !strings.Contains(conflicted, "two by e") {
		t.Fatalf("resolution worktree a.txt = %q", conflicted)
	}
	if err := done("w3", "merge-te"); err == nil || !strings.Contains(err.Error(), "conflict markers") {
		t.Fatalf("done with markers left: %v", err)
	}
	writeFile(t, rm.Worktree, "a.txt", "ONE\ntwo by d\ntwo by e\nthree\n")
	if err := done("w3", "merge-te"); err != nil {
		t.Fatal(err)
	}
	if show("a.txt") != "ONE\ntwo by d\ntwo by e\nthree" {
		t.Errorf("integration after resolution = %q", show("a.txt"))
	}

	// An unfinished attempt keeps its work on a renamed branch.
	rf := next("w1", "tf")
	writeFile(t, rf.Worktree, "half.txt", "wip\n")
	call("task.fail", TaskFailParams{ID: "tf", Agent: "w1", Reason: "gave up", Retryable: true}, nil)
	if got := git(t, dir, "show", "tazzy/tf-failed-1:half.txt"); got != "wip" {
		t.Errorf("kept attempt = %q", got)
	}

	for _, id := range []string{"w1", "w2", "w3"} {
		call("agent.leave", LeaveParams{Agent: id}, nil)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, ".tazzyronth", "worktrees")); len(ents) != 1 || ents[0].Name() != ".gitignore" {
		t.Errorf("worktrees left: %v", ents)
	}
	if wl := git(t, dir, "worktree", "list", "--porcelain"); strings.Count(wl, "worktree ") != 1 {
		t.Errorf("git worktree list:\n%s", wl)
	}
	branches := git(t, dir, "branch", "--format=%(refname:short)")
	for _, b := range []string{"tazzy/ta", "tazzy/tb", "tazzy/te", "tazzy/merge-te"} {
		if strings.Contains(branches+"\n", b+"\n") {
			t.Errorf("branch %s not deleted:\n%s", b, branches)
		}
	}
	if git(t, dir, "rev-parse", "main") != mainHead || git(t, dir, "symbolic-ref", "--short", "HEAD") != "main" ||
		git(t, dir, "status", "--porcelain", "--", ".", ":(exclude).tazzyronth") != "" {
		t.Error("the user's branch or checkout was touched")
	}
}

func TestWorktreeNeedsCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	call, callErr := wtHub(t, dir, config.Default())
	err := callErr("swarm.set", SwarmParams{Isolation: config.IsolationWorktree})
	if err == nil || !strings.Contains(err.Error(), "at least one commit") {
		t.Fatalf("swarm.set worktree in an empty repo: %v", err)
	}
	var s Snapshot
	call("status", nil, &s)
	if s.Swarm.Isolation != "" {
		t.Errorf("isolation recorded anyway: %+v", s.Swarm)
	}
}
