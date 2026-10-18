package hub

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/bus"
	"tazzyronth/internal/config"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/proto"
)

// failHub starts a hub with short stall timeouts and returns a client and a
// call helper that fails the test on error.
func failHub(t *testing.T, stall float64) (*Client, func(method string, params, result any)) {
	t.Helper()
	cfg := config.Default()
	cfg.Timeouts.Stall, cfg.Timeouts.StallGrace = stall, stall
	dir := t.TempDir()
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c, func(method string, params, result any) {
		t.Helper()
		if err := c.Call(ctx, method, params, result); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
}

func leadInbox(t *testing.T, call func(string, any, any)) []bus.Message {
	t.Helper()
	var msgs []bus.Message
	call("msg.inbox", AgentParams{Agent: "lead"}, &msgs)
	return msgs
}

func TestFailurePolicy(t *testing.T) {
	c, call := failHub(t, 300)
	call("agent.join", JoinParams{ID: "lead", Role: "lead"}, nil)
	call("agent.join", JoinParams{ID: "a"}, nil)
	call("agent.join", JoinParams{ID: "b"}, nil)
	call("task.add", TaskAddParams{Tasks: []graph.Task{
		{ID: "x", Priority: 1},
		{ID: "y", Deps: []string{"x"}},
		{ID: "z", Deps: []string{"y"}},
		{ID: "solo"},
	}}, nil)
	claim := func(agent, want string) {
		t.Helper()
		var got *graph.Task
		call("sched.next", AgentParams{Agent: agent}, &got)
		if got == nil || got.ID != want {
			t.Fatalf("%s claimed %+v, want %s", agent, got, want)
		}
	}
	fail := func(agent, task string, retry bool) FailResult {
		t.Helper()
		var r FailResult
		call("task.fail", TaskFailParams{ID: task, Agent: agent, Reason: "broke on " + agent, Retryable: retry}, &r)
		return r
	}

	// Attempt 1 on a: requeued, nothing for the lead yet.
	claim("a", "x")
	call("task.note", TaskNoteParams{ID: "x", Agent: "a", Text: "half done", Files: []string{"x.go"}}, nil)
	if r := fail("a", "x", true); r.Task.State != graph.Ready || len(r.Task.Failures) != 1 {
		t.Fatalf("after 1st failure: %+v", r)
	}
	if msgs := leadInbox(t, call); len(msgs) != 0 {
		t.Fatalf("lead told too early: %+v", msgs)
	}
	// a would rather take something it hasn't failed.
	claim("a", "solo")
	// Attempt 2 on b, a second agent: escalation, still requeued.
	claim("b", "x")
	if r := fail("b", "x", true); r.Task.State != graph.Ready || len(r.Task.Checkpoints) != 1 {
		t.Fatalf("after 2nd failure: %+v", r)
	}
	msgs := leadInbox(t, call)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "2 different agents") ||
		!strings.Contains(msgs[0].Body, "a: broke on a") || !strings.Contains(msgs[0].Body, "b: broke on b") {
		t.Fatalf("escalation = %+v", msgs)
	}
	call("msg.ack", AckParams{Agent: "lead", IDs: []string{msgs[0].ID}}, nil)
	// Attempt 3 of 3: fails for good, and so do its dependents.
	claim("b", "x")
	r := fail("b", "x", true)
	if r.Task.State != graph.Failed || strings.Join(r.Cascaded, ",") != "y,z" {
		t.Fatalf("after 3rd failure: %+v", r)
	}
	msgs = leadInbox(t, call)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "x failed after 3 attempt") || !strings.Contains(msgs[0].Body, "y, z") {
		t.Fatalf("final failure report = %+v", msgs)
	}
	var z graph.Task
	call("task.get", IDParams{ID: "z"}, &z)
	if z.State != graph.Failed || len(z.Failures) != 1 {
		t.Fatalf("z = %+v", z)
	}

	// Non-retryable: failed at once; only the owner may fail it.
	if err := c.Call(context.Background(), "task.fail", TaskFailParams{ID: "solo", Agent: "b", Reason: "not mine"}, nil); err == nil {
		t.Fatal("b failed a's task")
	}
	if r := fail("a", "solo", false); r.Task.State != graph.Failed {
		t.Fatalf("non-retryable: %+v", r)
	}
}

func TestStallAndLateReturn(t *testing.T) {
	c, call := failHub(t, 0.2)
	ctx := context.Background()
	call("agent.join", JoinParams{ID: "lead", Role: "lead"}, nil)
	call("agent.join", JoinParams{ID: "slow"}, nil)
	call("agent.join", JoinParams{ID: "zombie"}, nil)
	call("task.add", TaskAddParams{Tasks: []graph.Task{{ID: "p", Priority: 1, Files: []string{"p.go"}}, {ID: "q"}}}, nil)
	var task *graph.Task
	call("sched.next", AgentParams{Agent: "zombie"}, &task)
	call("lease.acquire", LeaseParams{Agent: "zombie", Paths: []string{"p.go"}}, nil)
	call("task.note", TaskNoteParams{ID: "p", Agent: "zombie", Text: "step 1", Files: []string{"p.go"}}, nil)
	call("sched.next", AgentParams{Agent: "slow"}, &task)

	// slow answers its ping in time; zombie never does.
	deadline := time.Now().Add(5 * time.Second)
	for answered := false; ; {
		if time.Now().After(deadline) {
			t.Fatal("zombie was never removed")
		}
		var msgs []bus.Message
		if !answered {
			var snap Snapshot
			call("status", nil, &snap)
			if q := snap.Queues["agent:slow"]; len(q) > 0 && q[0].Kind == "directive" && q[0].From == hubSender {
				call("msg.inbox", AgentParams{Agent: "slow"}, &msgs) // the heartbeat
				answered = true
			}
		}
		var p graph.Task
		call("task.get", IDParams{ID: "p"}, &p)
		if p.State == graph.Ready {
			if len(p.Checkpoints) != 1 || len(p.Failures) != 1 || !strings.Contains(p.Failures[0].Text, "stalled") {
				t.Fatalf("requeued p = %+v", p)
			}
			if !answered {
				t.Fatal("zombie removed before slow was pinged")
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var agents []Agent
	call("agent.list", nil, &agents)
	if len(agents) != 2 || agents[0].ID != "lead" || agents[1].ID != "slow" {
		t.Fatalf("agents = %+v, want lead and slow", agents)
	}
	var leases []any
	call("lease.list", LeaseListParams{}, &leases)
	if len(leases) != 0 {
		t.Fatalf("zombie's leases survived: %v", leases)
	}

	// The zombie comes back: every call is refused with a clear directive.
	for _, m := range []struct {
		method string
		params any
	}{
		{"task.state", TaskStateParams{ID: "p", State: graph.Done, Agent: "zombie"}},
		{"task.note", TaskNoteParams{ID: "p", Agent: "zombie", Text: "step 2"}},
		{"lease.acquire", LeaseParams{Agent: "zombie", Paths: []string{"p.go"}}},
		{"sched.next", AgentParams{Agent: "zombie"}},
	} {
		err := c.Call(ctx, m.method, m.params, nil)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != CodeRemoved || !strings.Contains(pe.Message, "stop working") {
			t.Fatalf("zombie %s: %v", m.method, err)
		}
	}
	// It may start over as a worker under its old id.
	call("agent.join", JoinParams{ID: "zombie"}, nil)
	call("sched.next", AgentParams{Agent: "zombie"}, &task)
	if task == nil || task.ID != "p" || len(task.Checkpoints) != 1 || task.Attempts != 2 {
		t.Fatalf("reclaim = %+v", task)
	}
}
