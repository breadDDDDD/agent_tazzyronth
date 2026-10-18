package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"tazzyronth/internal/bus"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/proto"
	"tazzyronth/internal/store"
)

// session is the shim's view of the hub: one connection (re-dialled when it
// drops), the default caller identity, and a change signal fed by the hub's
// event stream that blocking tools wait on.
//
// One shim may serve several agents (Claude Code subagents share their
// parent's MCP servers), so tools take an optional "agent" argument; the
// first identity registered here is the default.
type session struct {
	connect func(context.Context) (*hub.Client, error)
	log     *log.Logger
	connMu  sync.Mutex // serialises (re)connecting

	mu         sync.Mutex // guards everything below
	c          *hub.Client
	changed    chan struct{}       // closed and replaced on every hub event
	taskEvents int64               // count of task.add/task.state events seen
	agent      string              // default identity
	preset     string              // id the shim was started with (hub-spawned worker), if any
	mine       map[string]string   // task -> agent here that claimed it and hasn't finished
	notices    map[string][]string // agent -> undelivered notices
	client     string              // MCP client name from initialize (resolves spawn mode auto)
	launched   bool                // the dashboard was opened by this shim

	launch func() string // opens the dashboard; set before Serve, may be nil
}

func newSession(connect func(context.Context) (*hub.Client, error), logger *log.Logger) *session {
	return &session{connect: connect, log: logger, changed: make(chan struct{}), mine: map[string]string{}, notices: map[string][]string{}}
}

type agentKey struct{}

// withAgent makes ctx act as agent id for the rest of a tool call.
func withAgent(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, agentKey{}, id)
}

// hub returns the live connection, connecting and subscribing if needed.
func (s *session) hub(ctx context.Context) (*hub.Client, error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.mu.Lock()
	c := s.c
	s.mu.Unlock()
	if c != nil {
		return c, nil
	}
	c, err := s.connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("hub unavailable: %w", err)
	}
	events, err := c.Subscribe(ctx)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("hub unavailable: %w", err)
	}
	s.mu.Lock()
	s.c = c
	s.mu.Unlock()
	go s.pump(c, events)
	return c, nil
}

func (s *session) pump(c *hub.Client, events <-chan store.Event) {
	for ev := range events {
		s.mu.Lock()
		s.noteEvent(ev)
		s.signal()
		s.mu.Unlock()
	}
	s.mu.Lock()
	if s.c == c {
		s.c = nil
	}
	s.signal() // wake waiters so they reconnect
	s.mu.Unlock()
}

// noteEvent records what waiters and the piggybacked inbox care about.
// Called with s.mu held.
func (s *session) noteEvent(ev store.Event) {
	switch ev.Type {
	case "task.add":
		s.taskEvents++
	case "task.state":
		s.taskEvents++
		var p hub.TaskStateParams
		if json.Unmarshal(ev.Data, &p) == nil && p.State == graph.Cancelled && s.mine[p.ID] != "" {
			owner := s.mine[p.ID]
			delete(s.mine, p.ID)
			s.notices[owner] = append(s.notices[owner], fmt.Sprintf("task %s was cancelled: stop working on it", p.ID))
		}
	}
}

func (s *session) signal() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *session) close() {
	s.mu.Lock()
	c := s.c
	s.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// call makes a hub RPC, reconnecting once if the connection is gone.
func (s *session) call(ctx context.Context, method string, params, result any) error {
	for attempt := 0; ; attempt++ {
		c, err := s.hub(ctx)
		if err != nil {
			return err
		}
		err = c.Call(ctx, method, params, result)
		var pe *proto.Error
		if err == nil || errors.As(err, &pe) || ctx.Err() != nil || attempt > 0 {
			return err
		}
		s.log.Printf("hub call %s: %v; reconnecting", method, err)
		c.Close() // pump notices and clears s.c
		s.mu.Lock()
		if s.c == c {
			s.c = nil
		}
		s.mu.Unlock()
	}
}

// waitFor re-evaluates check after every hub event until it reports true,
// d elapses (false, nil) or ctx ends.
func (s *session) waitFor(ctx context.Context, d time.Duration, check func(context.Context) (bool, error)) (bool, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		if _, err := s.hub(ctx); err != nil { // must be subscribed before checking
			return false, err
		}
		s.mu.Lock()
		ch := s.changed
		s.mu.Unlock()
		if ok, err := check(ctx); ok || err != nil {
			return ok, err
		}
		select {
		case <-ch:
		case <-timer.C:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

var errNoAgent = errors.New("not registered: call tazzy_join (worker) or tazzy_start (lead) first")

// me is the calling agent: the call's "agent" argument, else the default.
func (s *session) me(ctx context.Context) (string, error) {
	if id, _ := ctx.Value(agentKey{}).(string); id != "" {
		return id, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == "" {
		return "", errNoAgent
	}
	return s.agent, nil
}

// setAgent records a registration; it becomes the default identity if
// override is set or there is none yet.
func (s *session) setAgent(id string, override bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if override || s.agent == "" {
		s.agent = id
	}
}

// setMine records that agent owns task here ("" = no longer).
func (s *session) setMine(task, agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if agent != "" {
		s.mine[task] = agent
	} else {
		delete(s.mine, task)
	}
}

// inboxMsg is a message as shown to the agent: no seq or timestamps.
type inboxMsg struct {
	ID      string `json:"id"`
	From    string `json:"from,omitempty"`
	To      string `json:"to"`
	Kind    string `json:"kind"`
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to,omitempty"`
}

type inbox struct {
	Messages []inboxMsg `json:"messages,omitempty"`
	Notices  []string   `json:"notices,omitempty"`
}

// pending reports whether the agent has anything to read, without consuming.
func (s *session) pending(ctx context.Context, agent string) (bool, error) {
	s.mu.Lock()
	n := len(s.notices[agent])
	s.mu.Unlock()
	if n > 0 {
		return true, nil
	}
	var msgs []bus.Message
	err := s.call(ctx, "msg.inbox", hub.AgentParams{Agent: agent}, &msgs)
	return len(msgs) > 0, err
}

// takeInbox fetches and acks the agent's unread messages and notices. It
// returns nil if there is nothing (or no agent yet). Delivery is effectively
// at-most-once from here: messages are acked before the reply is written.
func (s *session) takeInbox(ctx context.Context) *inbox {
	agent, err := s.me(ctx)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	notices := s.notices[agent]
	delete(s.notices, agent)
	s.mu.Unlock()
	in := &inbox{Notices: notices}
	var msgs []bus.Message
	if err := s.call(ctx, "msg.inbox", hub.AgentParams{Agent: agent}, &msgs); err != nil {
		s.log.Printf("inbox: %v", err)
	}
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
		in.Messages = append(in.Messages, inboxMsg{m.ID, m.From, m.To, m.Kind, m.Body, m.ReplyTo})
	}
	if len(ids) > 0 {
		if err := s.call(ctx, "msg.ack", hub.AckParams{Agent: agent, IDs: ids}, nil); err != nil {
			s.log.Printf("ack: %v", err)
		}
	}
	if len(in.Messages) == 0 && len(in.Notices) == 0 {
		return nil
	}
	return in
}

// content is an MCP text content block.
type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func textJSON(v any) content {
	b, _ := json.Marshal(v)
	return content{Type: "text", Text: string(b)}
}

// callTool runs a tool and appends the caller's inbox as a second content
// block. Tool failures are reported in-band (isError), as MCP prescribes.
func (s *session) callTool(ctx context.Context, t *tool, args json.RawMessage) toolResult {
	var out toolResult
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	var who struct{ Agent string }
	if json.Unmarshal(args, &who) == nil && who.Agent != "" {
		ctx = withAgent(ctx, who.Agent)
	}
	res, err := t.run(ctx, s, args)
	if err != nil {
		var pe *proto.Error
		msg := err.Error()
		if errors.As(err, &pe) {
			msg = pe.Message
		}
		out.IsError = true
		out.Content = []content{{Type: "text", Text: strings.TrimSpace(msg)}}
	} else {
		out.Content = []content{textJSON(res)}
	}
	if t.noInbox {
		return out
	}
	if in := s.takeInbox(ctx); in != nil {
		out.Content = append(out.Content, textJSON(map[string]any{"inbox": in}))
	}
	return out
}
