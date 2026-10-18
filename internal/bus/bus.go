// Package bus routes messages between agents. Each agent has an inbox of
// unacked messages; delivery is at-least-once (a message stays until acked)
// with dedup by message id.
//
// Addresses: agent:<id>, role:<name>, topic:<name>, broadcast, and task:<id>.
// Messages to a task wait in the task's own queue and show up in the inbox of
// whoever owns the task when it is read, so they follow reassignment.
package bus

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var Kinds = []string{"info", "question", "handoff", "blocker", "directive"}

type Message struct {
	ID      string    `json:"id"`
	Seq     int64     `json:"seq"`
	From    string    `json:"from,omitempty"`
	To      string    `json:"to"`
	Kind    string    `json:"kind"`
	Body    string    `json:"body"`
	ReplyTo string    `json:"reply_to,omitempty"`
	Time    time.Time `json:"time"`
}

var ErrNoRecipients = errors.New("no recipients")

// Bus is not safe for concurrent use.
type Bus struct {
	ownerOf func(task string) string // current owner, "" if none
	agents  map[string]string        // agent id -> role
	topics  map[string][]string      // topic -> subscribed agent ids
	queues  map[string][]Message     // "agent:<id>" or "task:<id>" -> unacked
	sent    map[string]Message       // every message ever sent, by id
	seq     int64
}

// New returns a bus that resolves task:<id> owners with ownerOf.
func New(ownerOf func(task string) string) *Bus {
	return &Bus{
		ownerOf: ownerOf,
		agents:  map[string]string{},
		topics:  map[string][]string{},
		queues:  map[string][]Message{},
		sent:    map[string]Message{},
	}
}

// Join registers (or re-registers) an agent with a role.
func (b *Bus) Join(agent, role string) { b.agents[agent] = role }

// Leave unregisters an agent: it gets no further role, topic or broadcast
// fan-out. Messages already queued for it stay (and are kept for a rejoin).
func (b *Bus) Leave(agent string) {
	delete(b.agents, agent)
	for topic, subs := range b.topics {
		b.topics[topic] = slices.DeleteFunc(subs, func(a string) bool { return a == agent })
	}
}

func (b *Bus) Subscribe(agent, topic string) error {
	if _, ok := b.agents[agent]; !ok {
		return fmt.Errorf("unknown agent %q", agent)
	}
	if topic == "" {
		return errors.New("topic is required")
	}
	if !slices.Contains(b.topics[topic], agent) {
		b.topics[topic] = append(b.topics[topic], agent)
	}
	return nil
}

// Sent returns a previously sent message by id.
func (b *Bus) Sent(id string) (Message, bool) {
	m, ok := b.sent[id]
	return m, ok
}

// Send assigns m a seq (and an id if it has none), stamps it with t and
// queues it for every recipient. Re-sending an id that was already sent is a
// no-op that returns the original message. Fan-out addresses never deliver
// back to the sender.
func (b *Bus) Send(m Message, t time.Time) (Message, error) {
	if m.ID != "" {
		if orig, ok := b.sent[m.ID]; ok {
			return orig, nil
		}
	}
	if m.Kind == "" {
		m.Kind = "info"
	}
	if !slices.Contains(Kinds, m.Kind) {
		return Message{}, fmt.Errorf("unknown message kind %q", m.Kind)
	}
	queues, err := b.resolve(m.To, m.From)
	if err != nil {
		return Message{}, err
	}
	b.seq++
	m.Seq, m.Time = b.seq, t
	for n := b.seq; m.ID == ""; n++ {
		if id := fmt.Sprintf("m%d", n); b.sent[id].ID == "" {
			m.ID = id
		}
	}
	b.sent[m.ID] = m
	for _, q := range queues {
		b.queues[q] = append(b.queues[q], m)
	}
	return m, nil
}

// resolve maps an address to the queue keys it delivers to.
func (b *Bus) resolve(to, from string) ([]string, error) {
	kind, name, _ := strings.Cut(to, ":")
	var agents []string
	switch kind {
	case "agent":
		if _, ok := b.agents[name]; !ok {
			return nil, fmt.Errorf("unknown agent %q", name)
		}
		return []string{to}, nil
	case "task":
		if name == "" {
			return nil, errors.New("task address needs an id")
		}
		return []string{to}, nil
	case "role":
		for id, role := range b.agents {
			if role == name {
				agents = append(agents, id)
			}
		}
	case "topic":
		agents = slices.Clone(b.topics[name])
	case "broadcast":
		for id := range b.agents {
			agents = append(agents, id)
		}
	default:
		return nil, fmt.Errorf("bad address %q (want agent:, role:, task:, topic: or broadcast)", to)
	}
	agents = slices.DeleteFunc(agents, func(id string) bool { return id == from })
	if len(agents) == 0 {
		return nil, fmt.Errorf("%w for %q", ErrNoRecipients, to)
	}
	slices.Sort(agents)
	for i, id := range agents {
		agents[i] = "agent:" + id
	}
	return agents, nil
}

// inboxQueues returns the queue keys an agent reads: its own plus those of
// every task it currently owns.
func (b *Bus) inboxQueues(agent string) []string {
	keys := []string{"agent:" + agent}
	for k := range b.queues {
		if task, ok := strings.CutPrefix(k, "task:"); ok && b.ownerOf(task) == agent {
			keys = append(keys, k)
		}
	}
	return keys
}

// Inbox returns the agent's unacked messages in seq order.
func (b *Bus) Inbox(agent string) []Message {
	var out []Message
	for _, k := range b.inboxQueues(agent) {
		out = append(out, b.queues[k]...)
	}
	slices.SortFunc(out, func(x, y Message) int { return int(x.Seq - y.Seq) })
	return out
}

// Ack removes the given messages from the agent's inbox and returns how many
// were removed. Unknown or already-acked ids are ignored.
func (b *Bus) Ack(agent string, ids []string) int {
	n := 0
	for _, k := range b.inboxQueues(agent) {
		q := b.queues[k]
		kept := slices.DeleteFunc(q, func(m Message) bool { return slices.Contains(ids, m.ID) })
		n += len(q) - len(kept)
		if len(kept) == 0 {
			delete(b.queues, k)
		} else {
			b.queues[k] = kept
		}
	}
	return n
}

// Queues returns a copy of every non-empty queue, for status and snapshots.
func (b *Bus) Queues() map[string][]Message {
	out := make(map[string][]Message, len(b.queues))
	for k, q := range b.queues {
		if len(q) > 0 {
			out[k] = slices.Clone(q)
		}
	}
	return out
}
