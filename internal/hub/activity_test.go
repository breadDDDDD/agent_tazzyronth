package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/activity"
	"tazzyronth/internal/config"
	"tazzyronth/internal/graph"
)

func actHub(t *testing.T, dir string, sweep float64) (*Hub, *Client, func(method string, params, result any)) {
	t.Helper()
	cfg := config.Default()
	cfg.Activity.Poll, cfg.Activity.Sweep = 0.02, sweep
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return h, c, func(method string, params, result any) {
		t.Helper()
		if err := c.Call(ctx, method, params, result); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
}

// waitActivity polls activity.list until one of agent's events matches.
func waitActivity(t *testing.T, call func(string, any, any), agent string, match func(activity.Activity) bool) activity.Activity {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var as []activity.Activity
		call("activity.list", ActivityListParams{Agent: agent, Limit: 500}, &as)
		for _, a := range as {
			if match(a) {
				return a
			}
		}
		if time.Now().After(deadline) {
			b, _ := json.MarshalIndent(as, "", "  ")
			t.Fatalf("no matching activity for %q; have:\n%s", agent, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestActivityCapture(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/a.go", "package a\n\nfunc A() {}\n")
	h, c, call := actHub(t, dir, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watcher, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	live, err := watcher.SubscribeActivity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := c.Subscribe(ctx) // a plain subscriber sees no activity events
	if err != nil {
		t.Fatal(err)
	}

	call("agent.join", JoinParams{ID: "lead", Role: "lead"}, nil)
	call("agent.join", JoinParams{ID: "w1"}, nil)
	call("agent.join", JoinParams{ID: "w2"}, nil)
	call("task.add", TaskAddParams{Tasks: []graph.Task{{ID: "t1", Files: []string{"src/a.go"}}}}, nil)
	call("sched.next", AgentParams{Agent: "w1"}, nil)
	call("lease.acquire", LeaseParams{Agent: "w1", Paths: []string{"src/a.go"}}, nil)

	// Intent from a progress note.
	call("task.note", TaskNoteParams{ID: "t1", Agent: "w1", Text: "now: making A return"}, nil)
	waitActivity(t, call, "w1", func(a activity.Activity) bool {
		return a.Kind == activity.Intent && a.Summary == "now: making A return" && a.Task == "t1"
	})

	// The diff engine attributes an edit of a leased file to its holder.
	writeFile(t, dir, "src/a.go", "package a\n\nfunc A() int {\n\treturn 1\n}\n")
	e := waitActivity(t, call, "w1", func(a activity.Activity) bool { return a.Kind == activity.Edit && a.Source == activity.SrcDiff })
	if e.Task != "t1" || e.Summary != "src/a.go +3 −1" || !slices.Equal(e.Files, []string{"src/a.go"}) {
		t.Errorf("edit = %+v", e)
	}
	var d struct{ Diff string }
	if err := json.Unmarshal(e.Detail, &d); err != nil || !strings.Contains(d.Diff, "-func A() {}\n+func A() int {\n+\treturn 1\n+}\n") {
		t.Errorf("diff detail = %s", e.Detail)
	}

	// Hook events: tazzy_join's result binds the subagent to w2.
	hook := func(payload string) HookResult {
		var r HookResult
		call("activity.hook", HookParams{Payload: json.RawMessage(payload)}, &r)
		return r
	}
	hook(`{"session_id":"S","agent_id":"sub-1","hook_event_name":"PostToolUse","tool_name":"mcp__tazzyronth__tazzy_join","tool_input":{},
		"tool_response":[{"type":"text","text":"{\"agent\":\"w2\",\"next\":\"...\"}"}]}`)
	r := hook(fmt.Sprintf(`{"session_id":"S","agent_id":"sub-1","hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":%q}}`, filepath.Join(dir, "src", "b.go")))
	if r.Agent != "w2" || !r.Recorded {
		t.Errorf("hook result = %+v", r)
	}
	b := waitActivity(t, call, "w2", func(a activity.Activity) bool { return a.Source == activity.SrcHook })
	if b.Kind != activity.Edit || !slices.Equal(b.Files, []string{"src/b.go"}) {
		t.Errorf("hook activity = %+v", b)
	}
	if r := hook(`{"session_id":"abcdef123456","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`); r.Agent != "host-abcdef12" {
		t.Errorf("unbound hook = %+v", r)
	}

	// Worker stream: decoded and tallied.
	w := h.streamTo("w1", "claude")
	fmt.Fprintln(w, `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"go test"}}],"usage":{"input_tokens":100,"output_tokens":20}}}`)
	fmt.Fprintln(w, `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":0.5,"usage":{"input_tokens":100,"output_tokens":20}}`)
	w.Close()
	waitActivity(t, call, "w1", func(a activity.Activity) bool { return a.Source == activity.SrcStream && a.Kind == activity.Cmd })
	waitActivity(t, call, "w1", func(a activity.Activity) bool { return a.Source == activity.SrcStream && a.Kind == activity.State })
	var snap Snapshot
	call("status", nil, &snap)
	if snap.Usage == nil || snap.Usage.Agents["w1"].Input != 100 || snap.Usage.Swarm.CostUSD != 0.5 {
		t.Errorf("usage = %+v", snap.Usage)
	}

	// Messages and done show up too; the diff is fetchable on demand.
	call("msg.send", SendParams{From: "w1", To: "role:lead", Body: "hi"}, nil)
	call("task.state", TaskStateParams{ID: "t1", State: graph.Done, Agent: "w1"}, nil)
	waitActivity(t, call, "w1", func(a activity.Activity) bool { return a.Kind == activity.Msg })
	waitActivity(t, call, "w1", func(a activity.Activity) bool { return a.Kind == activity.State && a.Summary == "task t1 done" })
	var detail json.RawMessage
	call("activity.detail", ActivityDetailParams{ID: e.ID}, &detail)
	if !strings.Contains(string(detail), "return 1") {
		t.Errorf("detail = %s", detail)
	}

	// The live stream carried the edit; the plain one only logged events.
	timeout := time.After(5 * time.Second)
	for got := false; !got; {
		select {
		case ev := <-live:
			var a activity.Activity
			got = ev.Type == evActivity && json.Unmarshal(ev.Data, &a) == nil && a.ID == e.ID
		case <-timeout:
			t.Fatal("edit not streamed to the activity subscriber")
		}
	}
	for len(plain) > 0 {
		if ev := <-plain; ev.Type == evActivity {
			t.Fatal("plain subscriber got an activity event")
		}
	}

	// Persisted: a restarted hub still has it.
	h.Close()
	_, _, call2 := actHub(t, dir, 0)
	waitActivity(t, call2, "w1", func(a activity.Activity) bool { return a.ID == e.ID && a.Summary == e.Summary })
}

func TestCancelFailsDependents(t *testing.T) {
	_, call := failHub(t, 300)
	call("agent.join", JoinParams{ID: "lead", Role: "lead"}, nil)
	call("task.add", TaskAddParams{Tasks: []graph.Task{{ID: "a"}, {ID: "b", Deps: []string{"a"}}, {ID: "c", Deps: []string{"b"}}, {ID: "d"}}}, nil)
	var r TaskStateResult
	call("task.state", TaskStateParams{ID: "a", State: graph.Cancelled}, &r)
	if r.State != graph.Cancelled || !slices.Equal(r.Cascaded, []string{"b", "c"}) {
		t.Errorf("result = %+v", r)
	}
	for _, id := range []string{"b", "c"} {
		var tk graph.Task
		call("task.get", IDParams{ID: id}, &tk)
		if tk.State != graph.Failed || len(tk.Failures) != 1 || tk.Failures[0].Text != "dependency a was cancelled" {
			t.Errorf("%s = %+v", id, tk)
		}
	}
	msgs := leadInbox(t, call)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "task a was cancelled; its dependents failed with it: b, c") {
		t.Errorf("lead inbox = %+v", msgs)
	}
	var d graph.Task
	call("task.get", IDParams{ID: "d"}, &d)
	if d.State != graph.Ready {
		t.Errorf("unrelated task = %s", d.State)
	}
}

func TestUnattributedEditAlert(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	writeFile(t, dir, "old-dirty.txt", "was dirty before the hub started\n")
	writeFile(t, dir, "ok.txt", "leased\n")
	_, _, call := actHub(t, dir, 0.05)
	call("agent.join", JoinParams{ID: "w1"}, nil)
	call("lease.acquire", LeaseParams{Agent: "w1", Paths: []string{"ok.txt"}}, nil)
	writeFile(t, dir, "ok.txt", "leased and edited\n")
	waitActivity(t, call, "w1", func(a activity.Activity) bool { return a.Kind == activity.Edit })

	// Keep touching the rogue file: the first sweep after it changes alerts.
	deadline := time.Now().Add(5 * time.Second)
	var alerts []activity.Activity
	for i := 0; len(alerts) == 0; i++ {
		if time.Now().After(deadline) {
			t.Fatal("no unattributed-edit alert")
		}
		writeFile(t, dir, "rogue.txt", fmt.Sprintf("edit %d\n", i))
		time.Sleep(30 * time.Millisecond)
		call("activity.list", ActivityListParams{Agent: alertAgent}, &alerts)
	}
	time.Sleep(200 * time.Millisecond) // a few more sweeps
	call("activity.list", ActivityListParams{Agent: alertAgent}, &alerts)
	for _, a := range alerts {
		if a.Kind != activity.Alert || !slices.Equal(a.Files, []string{"rogue.txt"}) {
			t.Errorf("alert = %+v", a)
		}
	}
}

// Host sessions report no usage in hook payloads: their tokens are read from
// the transcripts the payloads point at, once each, across hub restarts.
func TestHookTranscriptUsage(t *testing.T) {
	dir := t.TempDir()
	h, _, call := actHub(t, dir, 0)
	tdir := t.TempDir()
	sess := filepath.Join(tdir, "S.jsonl")
	line := func(id string, in, out int) string {
		return fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"content":[],"usage":{"input_tokens":%d,"output_tokens":%d}}}`+"\n", id, in, out)
	}
	writeFile(t, tdir, "S.jsonl", line("m1", 100, 10)+line("m1", 100, 10)+line("m2", 5, 1))
	writeFile(t, tdir, "S/subagents/agent-sub-1.jsonl", line("s1", 40, 4))
	hook := func(payload string) {
		t.Helper()
		var r HookResult
		call("activity.hook", HookParams{Payload: json.RawMessage(payload)}, &r)
	}
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	usage := func(call func(string, any, any)) map[string]activity.Tokens {
		var snap Snapshot
		call("status", nil, &snap)
		if snap.Usage == nil {
			return nil
		}
		return snap.Usage.Agents
	}
	// Bind the session to the lead and the subagent to w2.
	hook(`{"session_id":"S","hook_event_name":"PreToolUse","tool_name":"mcp__t__tazzy_status","tool_input":{"agent":"lead"}}`)
	hook(`{"session_id":"S","agent_id":"sub-1","hook_event_name":"PreToolUse","tool_name":"mcp__t__tazzy_next","tool_input":{"agent":"w2"}}`)

	hook(`{"session_id":"S","hook_event_name":"Stop","transcript_path":` + q(sess) + `}`)
	hook(`{"session_id":"S","agent_id":"sub-1","hook_event_name":"PostToolUse","tool_name":"Read","tool_input":{"file_path":"x"},"tool_response":{},"transcript_path":` + q(sess) + `}`)
	hook(`{"session_id":"S","hook_event_name":"PostToolUse","tool_name":"Read","tool_input":{"file_path":"x"},"tool_response":{},"transcript_path":` + q(sess) + `}`)
	u := usage(call)
	if u["lead"] != (activity.Tokens{Input: 105, Output: 11}) || u["w2"] != (activity.Tokens{Input: 40, Output: 4}) {
		t.Fatalf("usage = %+v", u)
	}

	// Appended lines are counted on the next event; a restarted hub resumes.
	f, _ := os.OpenFile(sess, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(line("m3", 1, 1))
	f.Close()
	hook(`{"session_id":"S","hook_event_name":"Stop","transcript_path":` + q(sess) + `}`)
	if got := usage(call)["lead"]; got != (activity.Tokens{Input: 106, Output: 12}) {
		t.Fatalf("after append: %+v", got)
	}
	h.Close()
	_, _, call2 := actHub(t, dir, 0)
	var r HookResult
	call2("activity.hook", HookParams{Agent: "lead", Payload: json.RawMessage(`{"session_id":"S","hook_event_name":"Stop","transcript_path":` + q(sess) + `}`)}, &r)
	if got := usage(call2)["lead"]; got != (activity.Tokens{Input: 106, Output: 12}) {
		t.Fatalf("after restart: %+v", got)
	}
}
