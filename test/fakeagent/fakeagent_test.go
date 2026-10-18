package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/activity"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/hub"
)

var tazzyExe, fakeExe string

// TestMain builds the real tazzy binary (workers' MCP configs run its "mcp")
// and this fake worker.
func TestMain(m *testing.M) {
	bin, err := os.MkdirTemp("", "tazzy-fake-")
	if err != nil {
		panic(err)
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	tazzyExe, fakeExe = filepath.Join(bin, "tazzy"+ext), filepath.Join(bin, "fakeagent"+ext)
	for exe, pkg := range map[string]string{tazzyExe: "tazzyronth/cmd/tazzy", fakeExe: "tazzyronth/test/fakeagent"} {
		if out, err := exec.Command("go", "build", "-o", exe, pkg).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", pkg, err, out)
			os.RemoveAll(bin)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(bin)
	os.Exit(code)
}

// startHub writes a config for the fake adapter and starts a hub in spawn
// mode "hub" over it.
func startHub(t *testing.T, maxAgents int, fakeArgs ...string) (string, *hub.Hub, *hub.Client) {
	t.Helper()
	return startHubIn(t, t.TempDir(), nil, maxAgents, fakeArgs...)
}

// startHubIn is startHub in dir, with extra config fields.
func startHubIn(t *testing.T, dir string, extra map[string]any, maxAgents int, fakeArgs ...string) (string, *hub.Hub, *hub.Client) {
	t.Helper()
	adapter := map[string]any{"cmd": append([]string{fakeExe, "-mcp", "{mcp_config}"}, fakeArgs...)}
	if slices.Contains(fakeArgs, "-claude-stream") {
		adapter["stream"] = "claude"
	}
	cfg := map[string]any{
		"max_agents":      maxAgents,
		"spawn_mode":      "hub",
		"default_adapter": "fake",
		"adapters":        map[string]any{"fake": adapter},
		"timeouts":        map[string]any{"idle_retire_s": 0.5, "spawn_cooldown_s": 0.05, "scale_tick_s": 0.05, "hub_idle_s": 60, "stall_s": 1, "stall_grace_s": 0.5},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	b, _ := json.Marshal(cfg)
	os.MkdirAll(hub.StateDir(dir), 0o700)
	if err := os.WriteFile(filepath.Join(hub.StateDir(dir), "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := hub.StartWith(dir, hub.Options{Exe: tazzyExe})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	c, err := hub.Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return dir, h, c
}

// workerLog reads the event log and returns the most hub-spawned workers
// alive at once and why each one left, in order.
func workerLog(t *testing.T, dir string) (maxLive int, left [][2]string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(hub.StateDir(dir), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for line := range strings.Lines(string(b)) {
		var ev struct {
			Type string
			Data json.RawMessage
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		var p struct{ ID, Agent, Adapter, Reason string }
		json.Unmarshal(ev.Data, &p)
		switch ev.Type {
		case "agent.join":
			if p.Adapter != "" {
				live[p.ID] = true // pre-registration
			}
		case "agent.leave":
			if live[p.Agent] {
				delete(live, p.Agent)
				left = append(left, [2]string{p.Agent, p.Reason})
			}
		}
		maxLive = max(maxLive, len(live))
	}
	return maxLive, left
}

func status(t *testing.T, c *hub.Client) hub.Snapshot {
	t.Helper()
	var s hub.Snapshot
	if err := c.Call(context.Background(), "status", nil, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// poll calls f every 20 ms until it returns true or the deadline passes.
func poll(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func spawnedLive(s hub.Snapshot) int {
	n := 0
	for _, a := range s.Agents {
		if a.Adapter != "" {
			n++
		}
	}
	return n
}

func allDone(s hub.Snapshot) bool {
	for _, t := range s.Tasks {
		if t.State != graph.Done {
			return false
		}
	}
	return len(s.Tasks) > 0
}

// TestElasticScaling runs a DAG whose parallel width goes 1 -> 3 -> 1 with
// max_agents 5: the hub must ramp up to exactly 3 workers, never more, and
// retire the idle ones.
func TestElasticScaling(t *testing.T) {
	dir, _, c := startHub(t, 5, "-work", "300ms")
	tasks := []graph.Task{
		{ID: "a", Files: []string{"a.go"}},
		{ID: "b1", Deps: []string{"a"}, Files: []string{"b1.go"}},
		{ID: "b2", Deps: []string{"a"}, Files: []string{"b2.go"}},
		{ID: "b3", Deps: []string{"a"}, Files: []string{"b3.go"}},
		{ID: "c", Deps: []string{"b1", "b2", "b3"}, Files: []string{"c.go"}},
	}
	if err := c.Call(context.Background(), "task.add", hub.TaskAddParams{Tasks: tasks}, nil); err != nil {
		t.Fatal(err)
	}

	poll(t, 30*time.Second, "all tasks done", func() bool { return allDone(status(t, c)) })
	poll(t, 10*time.Second, "idle workers retired", func() bool { return spawnedLive(status(t, c)) == 0 })

	maxLive, left := workerLog(t, dir)
	if maxLive != 3 {
		t.Errorf("max live workers = %d, want 3", maxLive)
	}
	// Every worker was retired for idleness, none died; each had its own run
	// dir with an MCP config naming its id, and a log.
	for _, l := range left {
		agent, reason := l[0], l[1]
		if reason != "idle" {
			t.Errorf("%s left: %q, want idle", agent, reason)
		}
		cfg, err := os.ReadFile(filepath.Join(hub.StateDir(dir), "run", agent, "mcp.json"))
		if err != nil || !strings.Contains(string(cfg), `"-agent",`) || !strings.Contains(string(cfg), `"`+agent+`"`) {
			t.Errorf("%s: mcp.json = %s, %v", agent, cfg, err)
		}
		if _, err := os.Stat(filepath.Join(hub.StateDir(dir), "logs", agent+".log")); err != nil {
			t.Error(err)
		}
	}
	t.Logf("max live %d, workers %v", maxLive, left)
}

// TestWorkerDeath: a worker that exits mid-task is noticed; its lease is
// released and the task requeued, so the next worker finishes it.
func TestWorkerDeath(t *testing.T) {
	dir, _, c := startHub(t, 1, "-work", "50ms", "-die-on", "x")
	if err := c.Call(context.Background(), "task.add", hub.TaskAddParams{Tasks: []graph.Task{{ID: "x", Files: []string{"x.go"}}}}, nil); err != nil {
		t.Fatal(err)
	}
	var s hub.Snapshot
	poll(t, 20*time.Second, "task x done", func() bool {
		s = status(t, c)
		return allDone(s)
	})
	var x graph.Task
	if err := c.Call(context.Background(), "task.get", hub.IDParams{ID: "x"}, &x); err != nil {
		t.Fatal(err)
	}
	if x.Owner == "a1" || x.Attempts != 2 {
		t.Errorf("task x: owner %s, attempts %d; want another worker on attempt 2", x.Owner, x.Attempts)
	}
	for _, a := range s.Agents {
		if a.ID == "a1" {
			t.Error("dead worker a1 is still registered")
		}
	}
	for _, l := range s.Leases {
		if l.Holder == "a1" {
			t.Errorf("dead worker still holds %s", l.Path)
		}
	}
	if _, left := workerLog(t, dir); len(left) == 0 || left[0][0] != "a1" || !strings.Contains(left[0][1], "exit status 3") {
		t.Errorf("leaves = %v, want a1 first with exit status 3", left)
	}
}

// TestWorkerStall: a hub-spawned worker that goes silent mid-task is pinged,
// then removed and its process tree killed; with max_agents 1 the task can
// only finish if the hung process stopped counting as live.
func TestWorkerStall(t *testing.T) {
	dir, _, c := startHub(t, 1, "-work", "50ms", "-hang-on", "x")
	if err := c.Call(context.Background(), "task.add", hub.TaskAddParams{Tasks: []graph.Task{{ID: "x", Files: []string{"x.go"}}}}, nil); err != nil {
		t.Fatal(err)
	}
	poll(t, 20*time.Second, "task x done", func() bool { return allDone(status(t, c)) })
	var x graph.Task
	if err := c.Call(context.Background(), "task.get", hub.IDParams{ID: "x"}, &x); err != nil {
		t.Fatal(err)
	}
	if x.Owner == "a1" || x.Attempts != 2 || len(x.Failures) != 1 || !strings.Contains(x.Failures[0].Text, "stalled") {
		t.Errorf("task x = %+v; want another worker on attempt 2 after a1 stalled", x)
	}
	if _, left := workerLog(t, dir); len(left) == 0 || left[0][0] != "a1" || !strings.HasPrefix(left[0][1], "stalled") {
		t.Errorf("leaves = %v, want a1 first, stalled", left)
	}
}

// TestActivityStream: a hub-spawned worker printing claude stream-json while
// editing its leased file shows up in the hub's activity as its tool calls,
// a correct diff of the edit attributed to it, its intent and its usage.
func TestActivityStream(t *testing.T) {
	dir, _, c := startHub(t, 1, "-work", "50ms", "-claude-stream")
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "src", "a.txt"), []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(context.Background(), "task.add", hub.TaskAddParams{Tasks: []graph.Task{{ID: "t1", Files: []string{"src/a.txt"}}}}, nil); err != nil {
		t.Fatal(err)
	}
	poll(t, 20*time.Second, "task t1 done", func() bool { return allDone(status(t, c)) })

	var as []activity.Activity
	find := func(match func(activity.Activity) bool) *activity.Activity {
		for i := range as {
			if match(as[i]) {
				return &as[i]
			}
		}
		return nil
	}
	var diffEdit *activity.Activity
	poll(t, 10*time.Second, "worker activity", func() bool {
		as = nil
		if err := c.Call(context.Background(), "activity.list", hub.ActivityListParams{Agent: "a1", Limit: 500}, &as); err != nil {
			t.Fatal(err)
		}
		diffEdit = find(func(a activity.Activity) bool { return a.Kind == activity.Edit && a.Source == activity.SrcDiff })
		return diffEdit != nil && find(func(a activity.Activity) bool { return a.Kind == activity.State && a.Source == activity.SrcStream }) != nil
	})
	path := filepath.Join(dir, "src", "a.txt")
	for _, want := range []struct{ kind, summary string }{
		{activity.Read, "Read " + path},
		{activity.Edit, "Edit " + path},
		{activity.Cmd, "Bash go test ./..."},
		{activity.CmdResult, "✖ Bash go test ./...: --- FAIL: TestX exit status 1"},
		{activity.Say, "Edited 1 file(s) for t1"},
	} {
		a := find(func(a activity.Activity) bool {
			return a.Source == activity.SrcStream && a.Kind == want.kind && a.Summary == want.summary
		})
		if a == nil {
			t.Errorf("no stream activity %s %q", want.kind, want.summary)
		} else if a.Task != "t1" {
			t.Errorf("%s: task %q", want.summary, a.Task)
		}
	}
	if e := find(func(a activity.Activity) bool { return a.Kind == activity.Edit && a.Source == activity.SrcStream }); e == nil || !slices.Equal(e.Files, []string{"src/a.txt"}) {
		t.Errorf("stream edit = %+v", e)
	}
	if find(func(a activity.Activity) bool { return a.Kind == activity.Intent && a.Summary == "working" }) == nil {
		t.Error("no intent from tazzy_progress")
	}

	var d struct{ Diff string }
	json.Unmarshal(diffEdit.Detail, &d)
	want := "--- a/src/a.txt\n+++ b/src/a.txt\n@@ -1,2 +1,3 @@\n line one\n+// edited by a1\n line two\n"
	if diffEdit.Summary != "src/a.txt +1 −0" || d.Diff != want || diffEdit.Task != "t1" {
		t.Errorf("diff edit = %+v\ndiff:\n%s", diffEdit, d.Diff)
	}

	u := status(t, c).Usage
	if u == nil || u.Agents["a1"].CostUSD != 0.25 || u.Agents["a1"].Input == 0 || u.Swarm.Output == 0 {
		t.Errorf("usage = %+v", u)
	}
}
