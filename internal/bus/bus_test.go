package bus

import (
	"errors"
	"slices"
	"testing"
	"time"
)

var t0 = time.Unix(0, 0).UTC()

func ids(ms []Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func newBus() (*Bus, map[string]string) {
	owners := map[string]string{}
	b := New(func(task string) string { return owners[task] })
	b.Join("lead", "lead")
	b.Join("w1", "worker")
	b.Join("w2", "worker")
	return b, owners
}

func TestAddressing(t *testing.T) {
	b, _ := newBus()
	b.Subscribe("w2", "api")
	b.Subscribe("lead", "api")

	tests := []struct {
		from, to string
		want     map[string]bool // which agents receive it
		err      bool
	}{
		{"lead", "agent:w1", map[string]bool{"w1": true}, false},
		{"w1", "role:lead", map[string]bool{"lead": true}, false},
		{"lead", "role:worker", map[string]bool{"w1": true, "w2": true}, false},
		{"w1", "role:worker", map[string]bool{"w2": true}, false}, // sender excluded
		{"w2", "topic:api", map[string]bool{"lead": true}, false},
		{"w1", "broadcast", map[string]bool{"lead": true, "w2": true}, false},
		{"lead", "agent:ghost", nil, true},
		{"lead", "role:nobody", nil, true},
		{"lead", "topic:none", nil, true},
		{"lead", "nonsense", nil, true},
	}
	for _, tt := range tests {
		before := map[string]int{}
		for _, a := range []string{"lead", "w1", "w2"} {
			before[a] = len(b.Inbox(a))
		}
		_, err := b.Send(Message{From: tt.from, To: tt.to, Body: "hi"}, t0)
		if (err != nil) != tt.err {
			t.Errorf("%s -> %s: err = %v, want err %v", tt.from, tt.to, err, tt.err)
			continue
		}
		for _, a := range []string{"lead", "w1", "w2"} {
			got := len(b.Inbox(a)) - before[a]
			if want := map[bool]int{true: 1}[tt.want[a]]; got != want {
				t.Errorf("%s -> %s: %s got %d new messages, want %d", tt.from, tt.to, a, got, want)
			}
		}
	}
	if _, err := b.Send(Message{To: "role:nobody"}, t0); !errors.Is(err, ErrNoRecipients) {
		t.Errorf("err = %v, want ErrNoRecipients", err)
	}
	if _, err := b.Send(Message{To: "agent:w1", Kind: "gossip"}, t0); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestDeliveryAckDedup(t *testing.T) {
	b, _ := newBus()
	m1, _ := b.Send(Message{From: "lead", To: "agent:w1", Body: "one"}, t0)
	m2, _ := b.Send(Message{ID: "client-7", From: "lead", To: "agent:w1", Body: "two"}, t0)
	if m1.ID == "" || m1.Seq != 1 || m2.ID != "client-7" || m2.Seq != 2 || m1.Kind != "info" {
		t.Fatalf("assigned: %+v %+v", m1, m2)
	}

	// Retrying the same id does not deliver twice.
	dup, err := b.Send(Message{ID: "client-7", From: "lead", To: "agent:w1", Body: "two (retry)"}, t0)
	if err != nil || dup.Body != "two" || dup.Seq != 2 {
		t.Fatalf("dup send = %+v, %v", dup, err)
	}
	if got := ids(b.Inbox("w1")); !slices.Equal(got, []string{m1.ID, "client-7"}) {
		t.Fatalf("inbox = %v", got)
	}

	// Unacked messages are redelivered on every read (at-least-once).
	if len(b.Inbox("w1")) != 2 {
		t.Fatal("read consumed messages")
	}
	if n := b.Ack("w1", []string{m1.ID, "unknown"}); n != 1 {
		t.Fatalf("Ack = %d, want 1", n)
	}
	if n := b.Ack("w1", []string{m1.ID}); n != 0 {
		t.Fatalf("double Ack = %d, want 0", n)
	}
	if got := ids(b.Inbox("w1")); !slices.Equal(got, []string{"client-7"}) {
		t.Fatalf("inbox after ack = %v", got)
	}
	// Acking in one inbox does not touch another's copy of a fan-out message.
	f, _ := b.Send(Message{From: "lead", To: "role:worker"}, t0)
	b.Ack("w1", []string{f.ID})
	if got := ids(b.Inbox("w2")); !slices.Equal(got, []string{f.ID}) {
		t.Fatalf("w2 inbox = %v", got)
	}
}

func TestTaskAddressFollowsOwner(t *testing.T) {
	b, owners := newBus()
	m, err := b.Send(Message{From: "lead", To: "task:t1", Body: "use v2 api"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Inbox("w1")) != 0 {
		t.Fatal("unowned task message delivered")
	}
	owners["t1"] = "w1"
	if got := ids(b.Inbox("w1")); !slices.Equal(got, []string{m.ID}) {
		t.Fatalf("w1 inbox = %v", got)
	}
	// Reassigned before ack: the message moves to the new owner.
	owners["t1"] = "w2"
	if len(b.Inbox("w1")) != 0 || len(b.Inbox("w2")) != 1 {
		t.Fatal("task message did not follow reassignment")
	}
	if b.Ack("w1", []string{m.ID}) != 0 {
		t.Fatal("old owner acked a task message")
	}
	if b.Ack("w2", []string{m.ID}) != 1 || len(b.Queues()) != 0 {
		t.Fatalf("queues after ack = %v", b.Queues())
	}
}
