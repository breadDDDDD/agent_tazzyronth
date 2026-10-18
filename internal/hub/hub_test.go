package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"tazzyronth/internal/board"
	"tazzyronth/internal/bus"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/lease"
	"tazzyronth/internal/proto"
	"tazzyronth/internal/store"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestReplayDeterminism drives a state through a realistic sequence (including
// rejected events, which must not be logged), then rebuilds a fresh state from
// the log and checks the two are identical.
func TestReplayDeterminism(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, _, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	live := newState("")
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	do := func(typ string, p any) {
		clock = clock.Add(time.Second)
		ev := store.Event{Time: clock, Type: typ, Data: json.RawMessage(mustJSON(t, p))}
		if _, err := live.apply(ev); err != nil {
			return // rejected: not logged, like Hub.mutate
		}
		if err := log.Append(&ev); err != nil {
			t.Fatal(err)
		}
	}

	do(evJoin, JoinParams{Role: "lead"})
	do(evJoin, JoinParams{Name: "w", Caps: []string{"go"}})
	do(evJoin, JoinParams{ID: "w9"})
	do(evTaskAdd, TaskAddParams{Tasks: []graph.Task{{ID: "a"}, {ID: "b", Deps: []string{"a"}}, {ID: "c", Deps: []string{"b"}}}})
	do(evTaskAdd, TaskAddParams{Tasks: []graph.Task{{ID: "x", Deps: []string{"x"}}}}) // cycle: rejected
	do(evTaskState, TaskStateParams{ID: "a", State: graph.Claimed, Agent: "a2"})
	do(evTaskNote, TaskNoteParams{ID: "a", Agent: "a2", Text: "half way"})
	do(evSend, SendParams{From: "a1", To: "task:b", Body: "heads up"})
	do(evSend, SendParams{From: "a1", To: "role:worker", Kind: "directive", Body: "go"})
	do(evSend, SendParams{ID: "q1", From: "a2", To: "role:lead", Kind: "question", Body: "jwt?"})
	do(evSend, SendParams{From: "a1", To: "agent:ghost"}) // rejected
	do(evTaskState, TaskStateParams{ID: "a", State: graph.Done, Result: &graph.Result{Summary: "ok", Artifacts: map[string]string{"api": "v1"}}})
	do(evTaskState, TaskStateParams{ID: "b", State: graph.Claimed, Agent: "w9"})
	do(evAck, AckParams{Agent: "w9", IDs: []string{"m1"}})
	do(evSubscribe, SubscribeParams{Agent: "a2", Topic: "api"})
	do(evBoardSet, BoardSetParams{Key: "api/schema", Value: "v1", By: "a2"})
	do(evBoardSet, BoardSetParams{Key: "api/schema", Value: "v2", By: "a2"})
	do(evDecide, DecideParams{Text: "JWT", By: "a1"})
	do(evLease, LeaseParams{Agent: "w9", Paths: []string{"src/b/**"}})
	do(evLease, LeaseParams{Agent: "a2", Paths: []string{"src/b/x.go"}, Mode: lease.Read}) // conflict: rejected
	do(evLease, LeaseParams{Agent: "a2", Paths: []string{"docs"}, Mode: lease.Read, TTL: 10})
	do(evJoin, JoinParams{Name: "temp"})
	do(evLease, LeaseParams{Agent: "a4", Paths: []string{"tmp"}})
	do(evLeave, LeaveParams{Agent: "a4"})
	do(evTaskAdd, TaskAddParams{Tasks: []graph.Task{{ID: "f"}, {ID: "g", Deps: []string{"f"}}}})
	do(evTaskState, TaskStateParams{ID: "f", State: graph.Claimed, Agent: "a2"})
	do(evTaskFail, TaskFailParams{ID: "f", Agent: "a2", Reason: "flaky", Retryable: true, MaxAttempts: 2})
	do(evTaskFail, TaskFailParams{ID: "f", Agent: "a2", Reason: "again"}) // not claimed: rejected
	do(evTaskState, TaskStateParams{ID: "f", State: graph.Claimed, Agent: "a2"})
	do(evTaskFail, TaskFailParams{ID: "f", Agent: "a2", Reason: "flaky", Retryable: true, MaxAttempts: 2}) // out of attempts
	log.Close()

	events, err := store.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	replayed := newState("")
	for _, ev := range events {
		if _, err := replayed.apply(ev); err != nil {
			t.Fatalf("replay seq %d: %v", ev.Seq, err)
		}
	}
	want, got := mustJSON(t, live.snapshot()), mustJSON(t, replayed.snapshot())
	if got != want {
		t.Fatalf("replayed state differs\nlive:     %s\nreplayed: %s", want, got)
	}
	// Sanity: the scenario actually did something.
	s := replayed.snapshot()
	if len(s.Agents) != 3 || len(s.Tasks) != 5 || s.Tasks[4].State != graph.Failed || s.Tasks[1].State != graph.Claimed || len(s.Decisions) != 1 || len(s.Leases) != 1 {
		t.Fatalf("unexpected final state: %s", got)
	}
}

func startHub(t *testing.T, dir string) *Hub {
	t.Helper()
	h, err := Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func rpcCode(err error) int {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return 0
}

func TestAuth(t *testing.T) {
	dir := t.TempDir()
	h := startHub(t, dir)
	info, err := ReadInfo(dir)
	if err != nil || info != h.Info() || info.Port == 0 || len(info.Token) != 64 {
		t.Fatalf("hub.json = %+v, %v (hub has %+v)", info, err, h.Info())
	}

	if _, err := Dial(info.Addr(), "wrong"); rpcCode(err) != CodeUnauthorized {
		t.Fatalf("bad token: err = %v, want unauthorized", err)
	}

	// Any method before hello is refused and the connection closed.
	nc, err := net.Dial("tcp", info.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	fmt.Fprintln(nc, `{"jsonrpc":"2.0","id":1,"method":"status"}`)
	r := proto.NewReader(bufio.NewReader(nc))
	m, err := r.Read()
	if err != nil || m.Error == nil || m.Error.Code != CodeUnauthorized {
		t.Fatalf("pre-hello call: %+v, %v", m, err)
	}
	if _, err := r.Read(); err == nil {
		t.Fatal("connection still open after unauthorized call")
	}

	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	if _, err := Start(dir); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Start: %v, want ErrRunning", err)
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	h := startHub(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	watcher, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	events, err := watcher.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}

	call := func(method string, params, result any) {
		t.Helper()
		if err := c.Call(ctx, method, params, result); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}

	var lead, w Agent
	call("agent.join", JoinParams{Role: "lead"}, &lead)
	call("agent.join", JoinParams{Name: "worker", Caps: []string{"go"}}, &w)
	if lead.ID != "a1" || w.ID != "a2" || w.Role != "worker" {
		t.Fatalf("agents = %+v %+v", lead, w)
	}

	call("task.add", TaskAddParams{Tasks: []graph.Task{{ID: "api", Title: "API"}, {ID: "ui", Deps: []string{"api"}}}}, nil)
	err = c.Call(ctx, "task.add", TaskAddParams{Tasks: []graph.Task{{ID: "x", Deps: []string{"y"}}, {ID: "y", Deps: []string{"x"}}}}, nil)
	if rpcCode(err) != CodeFailed {
		t.Fatalf("cycle: err = %v, want CodeFailed", err)
	}

	var ready []graph.Task
	call("task.list", TaskListParams{State: graph.Ready}, &ready)
	if len(ready) != 1 || ready[0].ID != "api" {
		t.Fatalf("ready = %+v", ready)
	}
	call("task.state", TaskStateParams{ID: "api", State: graph.Claimed, Agent: w.ID}, nil)

	var sent bus.Message
	call("msg.send", SendParams{ID: "c-1", From: lead.ID, To: "task:api", Body: "use JWT"}, &sent)
	var dup bus.Message
	call("msg.send", SendParams{ID: "c-1", From: lead.ID, To: "task:api", Body: "retry"}, &dup)
	if dup.Seq != sent.Seq || dup.Body != "use JWT" {
		t.Fatalf("dup send = %+v, want original %+v", dup, sent)
	}
	var inbox []bus.Message
	call("msg.inbox", AgentParams{Agent: w.ID}, &inbox)
	if len(inbox) != 1 || inbox[0].ID != "c-1" {
		t.Fatalf("inbox = %+v", inbox)
	}
	var acked map[string]int
	call("msg.ack", AckParams{Agent: w.ID, IDs: []string{"c-1"}}, &acked)
	if acked["acked"] != 1 {
		t.Fatalf("ack = %v", acked)
	}

	var done graph.Task
	call("task.state", TaskStateParams{ID: "api", State: graph.Done, Result: &graph.Result{Summary: "shipped"}}, &done)
	if done.Result == nil || done.Result.Summary != "shipped" {
		t.Fatalf("done = %+v", done)
	}
	var ui graph.Task
	call("task.get", IDParams{ID: "ui"}, &ui)
	if ui.State != graph.Ready {
		t.Fatalf("ui = %s, want ready", ui.State)
	}

	call("board.set", BoardSetParams{Key: "api/auth", Value: "JWT", By: w.ID}, nil)
	call("board.decide", DecideParams{Text: "JWT over sessions", By: lead.ID}, nil)
	var e *board.Entry
	call("board.get", KeyParams{Key: "api/auth"}, &e)
	if e == nil || e.Value != "JWT" {
		t.Fatalf("board.get = %+v", e)
	}
	var missing *board.Entry
	call("board.get", KeyParams{Key: "nope"}, &missing)
	if missing != nil {
		t.Fatalf("missing key = %+v", missing)
	}

	if err := c.Call(ctx, "no.such", nil, nil); rpcCode(err) != proto.CodeMethodNotFound {
		t.Fatalf("unknown method: %v", err)
	}
	if err := c.Call(ctx, "task.get", []int{1}, nil); rpcCode(err) != proto.CodeInvalidParams {
		t.Fatalf("bad params: %v", err)
	}

	// Every successful mutation reached the subscriber, in order. The
	// duplicate send and the rejected cycle are not events.
	wantTypes := []string{evJoin, evJoin, evTaskAdd, evTaskState, evSend, evAck, evTaskState, evBoardSet, evDecide}
	for i, want := range wantTypes {
		select {
		case ev := <-events:
			if ev.Type != want || ev.Seq != int64(i+1) {
				t.Fatalf("event %d = %s seq %d, want %s seq %d", i, ev.Type, ev.Seq, want, i+1)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for event %d (%s)", i, want)
		}
	}

	// Restarting the hub replays the log to the same state.
	var before, after Snapshot
	call("status", nil, &before)
	c.Close()
	watcher.Close()
	h.Close()
	if _, err := ReadInfo(dir); err == nil {
		t.Fatal("hub.json left behind after Close")
	}
	startHub(t, dir)
	c2, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.Call(ctx, "status", nil, &after); err != nil {
		t.Fatal(err)
	}
	if mustJSON(t, before) != mustJSON(t, after) {
		t.Fatalf("state changed across restart\nbefore: %s\nafter:  %s", mustJSON(t, before), mustJSON(t, after))
	}
}
