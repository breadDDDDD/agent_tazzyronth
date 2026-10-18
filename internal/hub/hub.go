// Package hub is the coordinator process that owns all swarm state. It speaks
// NDJSON JSON-RPC 2.0 over loopback TCP; every connection must first call
// "hello" with the token from .tazzyronth/hub.json. Mutations are appended to
// the event log and fanned out to "events.subscribe" subscribers.
package hub

import (
	"cmp"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"tazzyronth/internal/activity"
	"tazzyronth/internal/bus"
	"tazzyronth/internal/config"
	"tazzyronth/internal/diff"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/lease"
	"tazzyronth/internal/proto"
	"tazzyronth/internal/store"
	"tazzyronth/internal/supervisor"
	"tazzyronth/internal/web"
	"tazzyronth/internal/worktree"
)

// Application error codes (JSON-RPC reserves -32000..-32099 for servers).
const (
	CodeFailed       = -32000 // well-formed request, but the operation failed
	CodeUnauthorized = -32001
	CodeConflict     = -32002 // lease.acquire; Data holds the []lease.Conflict
	CodeRemoved      = -32003 // the agent named in the request was removed (dead, stalled, retired)
)

const (
	helloTimeout = 10 * time.Second
	subBuffer    = 256 // events queued per subscriber before it is dropped
	expireEvery  = 250 * time.Millisecond
)

var ErrRunning = errors.New("hub already running")

// Info is the discovery record written to .tazzyronth/hub.json.
type Info struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
	Web   int    `json:"web,omitempty"` // web view port, while it is running
}

func (i Info) Addr() string { return fmt.Sprintf("127.0.0.1:%d", i.Port) }

// StateDir is where the hub keeps hub.json and events.jsonl for a project.
func StateDir(projectDir string) string { return filepath.Join(projectDir, ".tazzyronth") }

func infoPath(projectDir string) string { return filepath.Join(StateDir(projectDir), "hub.json") }

func ReadInfo(projectDir string) (Info, error) {
	var info Info
	b, err := os.ReadFile(infoPath(projectDir))
	if err == nil {
		err = json.Unmarshal(b, &info)
	}
	return info, err
}

type HelloParams struct {
	Token string `json:"token"`
}

type HelloResult struct {
	PID int `json:"pid"`
}

type Hub struct {
	dir  string
	info Info
	ln   net.Listener
	wg   sync.WaitGroup

	cfg  config.Config
	sup  *supervisor.Supervisor
	stop chan struct{} // closed by Close
	idle chan struct{} // closed once the hub has been idle for cfg.Timeouts.HubIdle

	mu        sync.Mutex // guards everything below
	st        *state
	log       *store.Log
	conns     map[*conn]struct{}
	seen      map[string]time.Time // last request naming each agent; not logged
	pinged    map[string]time.Time // agent -> when the stall ping went out, until it is heard from
	idleSince map[string]time.Time // hub-spawned worker -> when it last had no task
	lastSpawn time.Time
	doneFrom  time.Time // since when every task has ended (zero while work remains)
	quietFrom time.Time // since when there have been no connections and no workers
	closed    bool
	act       *activity.Store
	watch     *diff.Watcher                 // snapshots of write-leased files
	hookIDs   map[string]string             // host hook identity -> agent (plan §3.9 identity binding)
	taps      map[chan store.Event]struct{} // in-process subscribers (the web view)
	web       *web.Server
	repo      *worktree.Repo // worktree isolation, once used
}

// Options tune Start. Zero values mean defaults.
type Options struct {
	Exe    string         // tazzy executable that spawned workers' MCP configs run; default os.Executable
	Config *config.Config // default: config.Load(projectDir)
	Web    bool           // serve the web view from the start (also config "web")
}

type conn struct {
	nc       net.Conn
	w        *proto.Writer
	events   chan store.Event // non-nil once subscribed; guarded by Hub.mu
	activity bool             // subscriber also wants activity events
}

// Start replays the project's event log, listens on a random loopback port
// and writes hub.json. It returns ErrRunning if hub.json points at a live hub.
func Start(projectDir string) (*Hub, error) { return StartWith(projectDir, Options{}) }

func StartWith(projectDir string, opt Options) (*Hub, error) {
	cfg := opt.Config
	if cfg == nil {
		c, err := config.Load(projectDir)
		if err != nil {
			return nil, err
		}
		cfg = &c
	}
	if opt.Exe == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		opt.Exe = exe
	}
	if info, err := ReadInfo(projectDir); err == nil {
		if c, err := Dial(info.Addr(), info.Token); err == nil {
			c.Close()
			return nil, fmt.Errorf("%w (pid %d, port %d)", ErrRunning, info.PID, info.Port)
		}
	}

	log, events, err := store.Open(filepath.Join(StateDir(projectDir), "events.jsonl"))
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(projectDir)
	if err != nil {
		log.Close()
		return nil, err
	}
	st := newState(root)
	for _, ev := range events {
		if _, err := st.apply(ev); err != nil {
			log.Close()
			return nil, fmt.Errorf("replay event %d (%s): %w", ev.Seq, ev.Type, err)
		}
	}

	act, err := activity.Open(filepath.Join(StateDir(projectDir), "activity"), cfg.Activity.Ring)
	if err != nil {
		log.Close()
		return nil, err
	}

	tok := make([]byte, 32)
	rand.Read(tok)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Close()
		act.Close()
		return nil, err
	}
	h := &Hub{
		dir:       projectDir,
		info:      Info{Port: ln.Addr().(*net.TCPAddr).Port, Token: hex.EncodeToString(tok), PID: os.Getpid()},
		ln:        ln,
		cfg:       *cfg,
		stop:      make(chan struct{}),
		idle:      make(chan struct{}),
		st:        st,
		log:       log,
		conns:     map[*conn]struct{}{},
		seen:      map[string]time.Time{},
		pinged:    map[string]time.Time{},
		idleSince: map[string]time.Time{},
		quietFrom: time.Now(),
		act:       act,
		watch:     diff.NewWatcher(root),
		hookIDs:   map[string]string{},
		taps:      map[chan store.Event]struct{}{},
	}
	h.sup = supervisor.New(root, opt.Exe, h.workerExited)
	h.sup.SetStdout(h.streamTo)
	h.watch.Dir = h.workdir
	h.watch.Sync(st.leases.List())
	// Workers from a previous hub died with it (their Job Object closed).
	for _, a := range st.agentList() {
		if a.Adapter != "" {
			if _, err := h.leaveLocked(a.ID, "hub restarted"); err != nil {
				ln.Close()
				log.Close()
				act.Close()
				return nil, err
			}
		}
	}
	h.cleanTreesLocked()
	if err := writeInfo(projectDir, h.info); err != nil {
		ln.Close()
		log.Close()
		act.Close()
		return nil, err
	}
	register(Running{Dir: root, PID: h.info.PID, Port: h.info.Port})
	h.wg.Add(4)
	go h.accept()
	go h.monitorLoop()
	go h.scaleLoop()
	go h.activityLoop()
	if cfg.Web || opt.Web {
		if _, err := h.StartWeb(); err != nil {
			h.Close()
			return nil, fmt.Errorf("web view: %w", err)
		}
	}
	return h, nil
}

func writeInfo(projectDir string, info Info) error {
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	path := infoPath(projectDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (h *Hub) Info() Info {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.info
}

// Idle is closed once the hub has had no connections and no workers for the
// configured hub_idle_s; the owner should then Close it.
func (h *Hub) Idle() <-chan struct{} { return h.idle }

// Close kills the workers, stops accepting, drops every connection, removes
// hub.json and closes the event log.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	for c := range h.conns {
		c.nc.Close()
	}
	for ch := range h.taps {
		h.untapLocked(ch)
	}
	ws := h.web
	h.mu.Unlock()
	close(h.stop)
	if ws != nil {
		ws.Close()
	}
	h.ln.Close()
	h.sup.Close() // their exits aren't logged; the next Start cleans them up
	h.wg.Wait()
	if info, err := ReadInfo(h.dir); err == nil && info.Token == h.info.Token {
		os.Remove(infoPath(h.dir))
	}
	unregister(Running{Dir: h.st.root, PID: h.info.PID, Port: h.info.Port})
	return errors.Join(h.act.Close(), h.log.Close())
}

func (h *Hub) accept() {
	defer h.wg.Done()
	for {
		nc, err := h.ln.Accept()
		if err != nil {
			return // listener closed
		}
		c := &conn{nc: nc, w: proto.NewWriter(nc)}
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			nc.Close()
			return
		}
		h.conns[c] = struct{}{}
		h.wg.Add(1)
		h.mu.Unlock()
		go h.serve(c)
	}
}

// serve handles one connection. Requests are processed in order, one at a time.
func (h *Hub) serve(c *conn) {
	defer h.wg.Done()
	defer func() {
		h.mu.Lock()
		delete(h.conns, c)
		if c.events != nil {
			close(c.events)
		}
		h.mu.Unlock()
		c.nc.Close()
	}()

	r := proto.NewReader(c.nc)
	c.nc.SetReadDeadline(time.Now().Add(helloTimeout))
	authed := false
	for {
		m, err := r.Read()
		if err != nil {
			var pe *proto.Error
			if errors.As(err, &pe) {
				c.w.Write(proto.NewError(nil, pe))
				continue
			}
			return
		}
		if !m.IsRequest() {
			continue // the hub sends no requests, and client notifications mean nothing yet
		}
		if !authed {
			var p HelloParams
			if m.Method != "hello" || json.Unmarshal(m.Params, &p) != nil ||
				subtle.ConstantTimeCompare([]byte(p.Token), []byte(h.info.Token)) != 1 {
				c.w.Write(proto.NewError(m.ID, &proto.Error{Code: CodeUnauthorized, Message: "unauthorized: call hello with the token from hub.json"}))
				return
			}
			authed = true
			c.nc.SetReadDeadline(time.Time{})
			h.reply(c, m.ID, HelloResult{PID: h.info.PID}, nil)
			continue
		}
		res, err := h.call(c, m.Method, m.Params)
		h.reply(c, m.ID, res, err)
	}
}

func (h *Hub) reply(c *conn, id json.RawMessage, res any, err error) {
	if err == nil {
		var resp *proto.Message
		if resp, err = proto.NewResult(id, res); err == nil {
			c.w.Write(resp)
			return
		}
	}
	var pe *proto.Error
	if !errors.As(err, &pe) {
		pe = &proto.Error{Code: CodeFailed, Message: err.Error()}
	}
	c.w.Write(proto.NewError(id, pe))
}

func (h *Hub) call(c *conn, method string, params json.RawMessage) (any, error) {
	f := methods[method]
	if f == nil {
		return nil, &proto.Error{Code: proto.CodeMethodNotFound, Message: "unknown method " + method}
	}
	if err := h.touch(method, params); err != nil {
		return nil, err
	}
	return f(h, c, params)
}

// touch records activity by the agent a request names: any request is a
// heartbeat (plan §3.4, §3.7) and renews the agent's leases. A request from
// an agent that has been removed is refused, so a stalled agent that comes
// back can't touch work that was given to someone else; only a join (which
// starts it over) gets through.
func (h *Hub) touch(method string, params json.RawMessage) error {
	var who struct{ Agent, From, By string }
	json.Unmarshal(params, &who) // best effort: most params are objects
	id := cmp.Or(who.Agent, who.From, who.By)
	if id == "" || passive[method] {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if why := h.st.left[id]; why != "" && method != evJoin {
		if released[why] {
			return &proto.Error{Code: CodeRemoved, Message: fmt.Sprintf(
				"agent %s was released (%s): there is no work left for it. Stop now and return a one-line summary; do not call tazzy_join again", id, why)}
		}
		return &proto.Error{Code: CodeRemoved, Message: fmt.Sprintf(
			"agent %s was removed (%s): stop working; any task and leases it held were given to others. Call tazzy_join to start over as a new worker", id, why)}
	}
	h.seen[id] = time.Now()
	delete(h.pinged, id)
	return nil
}

// passive methods name an agent without acting as it.
var passive = map[string]bool{"activity.list": true, "activity.hook": true}

// monitorLoop runs lease expiry and stall detection.
func (h *Hub) monitorLoop() {
	defer h.wg.Done()
	every := min(expireEvery, config.Seconds(h.cfg.Timeouts.Stall)/4, config.Seconds(h.cfg.Timeouts.StallGrace)/4)
	t := time.NewTicker(max(every, time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case now := <-t.C:
			h.mu.Lock()
			h.expire(now)
			h.stalls(now)
			h.mu.Unlock()
		}
	}
}

// stalls pings every agent that holds work (an active task or a lease) but
// has been silent for stall_s, and removes it if it is still silent
// stall_grace_s after the ping: its leases are released, its task requeued
// and, if the hub spawned it, its process tree killed. Called with h.mu held.
func (h *Hub) stalls(now time.Time) {
	stall, grace := config.Seconds(h.cfg.Timeouts.Stall), config.Seconds(h.cfg.Timeouts.StallGrace)
	busy := map[string]bool{}
	for _, t := range h.st.graph.List() {
		if active(t.State) {
			busy[t.Owner] = true
		}
	}
	for _, l := range h.st.leases.List() {
		busy[l.Holder] = true
	}
	for _, id := range slices.Sorted(maps.Keys(busy)) {
		seen, ok := h.seen[id]
		if h.st.agents[id] == nil {
			continue
		}
		if !ok { // e.g. just restarted: start the clock now
			h.seen[id] = now
			continue
		}
		pinged, wasPinged := h.pinged[id]
		switch {
		case wasPinged && now.Sub(pinged) >= grace:
			delete(h.pinged, id)
			if err := h.retireLocked(id, fmt.Sprintf("stalled: silent for %s", now.Sub(seen).Round(time.Millisecond))); err != nil {
				fmt.Fprintf(os.Stderr, "tazzy hub: remove stalled %s: %v\n", id, err)
			}
		case !wasPinged && now.Sub(seen) >= stall:
			h.pinged[id] = now
			body := fmt.Sprintf("no activity from you for %s: call any tazzy tool within %s or you will be removed and your task reassigned", now.Sub(seen).Round(time.Second), grace)
			if _, err := h.mutateLocked(evSend, SendParams{From: hubSender, To: "agent:" + id, Kind: "directive", Body: body}); err != nil {
				fmt.Fprintf(os.Stderr, "tazzy hub: ping %s: %v\n", id, err)
			}
		}
	}
}

// expire releases leases whose holder has been silent for longer than the
// lease's TTL. The release is an ordinary logged event, so replay doesn't
// depend on the clock. Called with h.mu held.
func (h *Hub) expire(now time.Time) {
	stale := map[string][]string{}
	for _, l := range h.st.leases.List() {
		seen, ok := h.seen[l.Holder]
		if !ok { // e.g. just restarted: start the clock now
			h.seen[l.Holder] = now
			continue
		}
		if now.Sub(seen) > time.Duration(l.TTL*float64(time.Second)) {
			stale[l.Holder] = append(stale[l.Holder], l.Path)
		}
	}
	for _, agent := range slices.Sorted(maps.Keys(stale)) {
		if _, err := h.mutateLocked(evRelease, LeaseReleaseParams{Agent: agent, Paths: stale[agent], Reason: "expired"}); err != nil {
			fmt.Fprintf(os.Stderr, "tazzy hub: expire leases of %s: %v\n", agent, err)
		}
	}
}

// mutate applies a state change, logs it and fans it out to subscribers.
func (h *Hub) mutate(typ string, params any) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mutateLocked(typ, params)
}

func (h *Hub) mutateLocked(typ string, params any) (any, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	ev := store.Event{Time: time.Now().UTC(), Type: typ, Data: data}
	res, err := h.st.apply(ev)
	if err != nil {
		return nil, err
	}
	if err := h.log.Append(&ev); err != nil {
		// State already moved on; it will be lost on restart. Nothing better
		// to do than tell the caller.
		return nil, fmt.Errorf("event log append: %w", err)
	}
	h.publishLocked(ev, false)
	h.afterLocked(ev, res)
	return res, nil
}

// publishLocked fans ev out to subscribers (only those that asked for
// activity, if isActivity). Called with h.mu held.
func (h *Hub) publishLocked(ev store.Event, isActivity bool) {
	for c := range h.conns {
		if c.events == nil || isActivity && !c.activity {
			continue
		}
		select {
		case c.events <- ev:
		default:
			c.nc.Close() // slow subscriber: drop it rather than block the hub
		}
	}
	for ch := range h.taps {
		select {
		case ch <- ev:
		default:
			h.untapLocked(ch) // slow: the web client reconnects
		}
	}
}

func (h *Hub) subscribe(c *conn, activity bool) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.activity = c.activity || activity
	if c.events == nil {
		c.events = make(chan store.Event, subBuffer)
		h.wg.Add(1)
		go func(ch <-chan store.Event) {
			defer h.wg.Done()
			for ev := range ch {
				m, err := proto.NewNotification("event", ev)
				if err != nil || c.w.Write(m) != nil {
					c.nc.Close()
					return
				}
			}
		}(c.events)
	}
	return h.log.Seq()
}

type method func(h *Hub, c *conn, params json.RawMessage) (any, error)

func decodeParams[T any](raw json.RawMessage) (T, error) {
	var p T
	if len(raw) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, &proto.Error{Code: proto.CodeInvalidParams, Message: err.Error()}
	}
	return p, nil
}

func mutation[T any](typ string) method {
	return func(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
		p, err := decodeParams[T](raw)
		if err != nil {
			return nil, err
		}
		return h.mutate(typ, p)
	}
}

func query[T any](f func(s *state, p T) (any, error)) method {
	return func(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
		p, err := decodeParams[T](raw)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		return f(h.st, p)
	}
}

// Query params.
type (
	IDParams struct {
		ID string `json:"id"`
	}
	TaskListParams struct {
		State graph.State `json:"state,omitempty"` // filter; empty = all
	}
	AgentParams struct {
		Agent string `json:"agent"`
	}
	KeyParams struct {
		Key string `json:"key"`
	}
	PrefixParams struct {
		Prefix string `json:"prefix,omitempty"`
	}
	LeaseCheckParams struct {
		Agent string     `json:"agent,omitempty"` // whose own leases don't count
		Paths []string   `json:"paths"`
		Mode  lease.Mode `json:"mode,omitempty"` // intended mode; default write
	}
	LeaseListParams struct {
		Agent string `json:"agent,omitempty"` // filter; empty = all
	}
)

var methods = map[string]method{
	evJoin:            mutation[JoinParams](evJoin),
	evLeave:           agentLeave,
	evTaskFail:        taskFail,
	evLease:           mutation[LeaseParams](evLease),
	evRelease:         mutation[LeaseReleaseParams](evRelease),
	"lease.check":     query(leaseCheck),
	"lease.list":      query(leaseList),
	"sched.next":      schedNext,
	evSwarm:           swarmSet,
	"agent.spawn":     agentSpawn,
	"agent.retire":    agentRetire,
	"agent.prune":     agentPrune,
	evTaskAdd:         mutation[TaskAddParams](evTaskAdd),
	evTaskState:       taskState,
	evTaskNote:        mutation[TaskNoteParams](evTaskNote),
	evAck:             mutation[AckParams](evAck),
	evSubscribe:       mutation[SubscribeParams](evSubscribe),
	evBoardSet:        mutation[BoardSetParams](evBoardSet),
	evDecide:          mutation[DecideParams](evDecide),
	evSend:            send,
	"agent.list":      query(func(s *state, _ struct{}) (any, error) { return s.agentList(), nil }),
	"task.get":        query(taskGet),
	"task.list":       query(taskList),
	"msg.inbox":       query(inbox),
	"board.get":       query(boardGet),
	"board.list":      query(boardList),
	"status":          status,
	"activity.list":   activityList,
	"activity.detail": activityDetail,
	"activity.hook":   activityHook,
	"web.start":       webStart,
	"events.subscribe": func(h *Hub, c *conn, raw json.RawMessage) (any, error) {
		p, err := decodeParams[EventsParams](raw)
		if err != nil {
			return nil, err
		}
		return map[string]int64{"seq": h.subscribe(c, p.Activity)}, nil
	},
}

// send short-circuits retries of an already-sent id so they aren't logged.
func send(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
	p, err := decodeParams[SendParams](raw)
	if err != nil {
		return nil, err
	}
	if p.ID != "" {
		h.mu.Lock()
		m, ok := h.st.bus.Sent(p.ID)
		h.mu.Unlock()
		if ok {
			return m, nil
		}
	}
	return h.mutate(evSend, p)
}

// schedNext scores the ready tasks for an agent and claims the best one in a
// single step, so two agents can't be handed the same task. Result: the
// claimed task, or null if nothing fits.
func schedNext(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
	p, err := decodeParams[AgentParams](raw)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, _, err := h.integrationLocked(); err != nil {
		return nil, err
	}
	id, err := h.st.pick(p.Agent, h.cfg.Scheduler.Weights)
	if err != nil || id == "" {
		return nil, err
	}
	res, err := h.mutateLocked(evTaskState, TaskStateParams{ID: id, State: graph.Claimed, Agent: p.Agent})
	if err != nil {
		return nil, err
	}
	r := res.(TaskStateResult)
	if err := h.startLocked(&r); err != nil {
		return nil, err
	}
	return r, nil
}

func leaseCheck(s *state, p LeaseCheckParams) (any, error) {
	mode, err := checkMode(p.Mode)
	if err != nil {
		return nil, err
	}
	paths, err := s.normalizeFor(p.Agent, p.Paths)
	if err != nil {
		return nil, err
	}
	c := s.leases.Conflicts(p.Agent, paths, mode)
	if c == nil {
		c = []lease.Conflict{}
	}
	return map[string]any{"conflicts": c}, nil
}

func leaseList(s *state, p LeaseListParams) (any, error) {
	if p.Agent != "" {
		return s.leases.Held(p.Agent), nil
	}
	return s.leases.List(), nil
}

func taskGet(s *state, p IDParams) (any, error) {
	t, ok := s.graph.Get(p.ID)
	if !ok {
		return nil, fmt.Errorf("%w: %q", graph.ErrNotFound, p.ID)
	}
	return t, nil
}

func taskList(s *state, p TaskListParams) (any, error) {
	out := []graph.Task{}
	for _, t := range s.graph.List() {
		if p.State == "" || t.State == p.State {
			out = append(out, t)
		}
	}
	return out, nil
}

func inbox(s *state, p AgentParams) (any, error) {
	if s.agents[p.Agent] == nil {
		return nil, fmt.Errorf("unknown agent %q", p.Agent)
	}
	msgs := s.bus.Inbox(p.Agent)
	if msgs == nil {
		msgs = []bus.Message{}
	}
	return msgs, nil
}

// boardGet returns null for a missing key.
func boardGet(s *state, p KeyParams) (any, error) {
	if e, ok := s.board.Get(p.Key); ok {
		return e, nil
	}
	return nil, nil
}

func boardList(s *state, p PrefixParams) (any, error) {
	return map[string]any{"entries": s.board.List(p.Prefix), "decisions": s.board.Decisions()}, nil
}

// hubSender is the From of messages the hub itself sends.
const hubSender = "hub"

func agentLeave(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
	p, err := decodeParams[LeaveParams](raw)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.leaveLocked(p.Agent, p.Reason)
}

// taskFail applies the failure policy (plan §3.7) with the configured retry
// limit and tells the lead when a task fails for good or keeps failing.
func taskFail(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
	p, err := decodeParams[TaskFailParams](raw)
	if err != nil {
		return nil, err
	}
	p.MaxAttempts = h.cfg.Retries.MaxAttempts
	h.mu.Lock()
	defer h.mu.Unlock()
	before, _ := h.st.graph.Get(p.ID)
	res, err := h.mutateLocked(evTaskFail, p)
	if err != nil {
		return nil, err
	}
	h.abandonLocked(before.Owner, before)
	h.reportLocked(res.(FailResult))
	return res, nil
}

// leaveLocked removes an agent, requeueing (or, out of attempts, failing)
// its tasks. Called with h.mu held.
func (h *Hub) leaveLocked(agent, reason string) (LeaveResult, error) {
	res, err := h.mutateLocked(evLeave, LeaveParams{Agent: agent, Reason: reason, MaxAttempts: h.cfg.Retries.MaxAttempts})
	if err != nil {
		return LeaveResult{}, err
	}
	lr := res.(LeaveResult)
	for _, r := range lr.Tasks {
		h.reportLocked(r)
		h.abandonLocked(agent, r.Task)
	}
	h.removeTreeLocked(agent)
	return lr, nil
}

// reportLocked messages role:lead about a task that failed for good, or that
// has just failed on its escalate_after-th different agent, with every
// failure reason so far. Called with h.mu held.
func (h *Hub) reportLocked(r FailResult) {
	t := r.Task
	var body string
	switch {
	case t.State == graph.Failed:
		body = fmt.Sprintf("task %s failed after %d attempt(s)", t.ID, t.Attempts)
		if len(r.Cascaded) > 0 {
			body += "; its dependents failed with it: " + strings.Join(r.Cascaded, ", ")
		}
	case escalated(t, h.cfg.Retries.EscalateAfter):
		body = fmt.Sprintf("task %s has failed on %d different agents and was queued again; consider re-planning it", t.ID, h.cfg.Retries.EscalateAfter)
	default:
		return
	}
	reasons := make([]string, len(t.Failures))
	for i, f := range t.Failures {
		reasons[i] = cmp.Or(f.Agent, "?") + ": " + f.Text
	}
	h.notifyLeadLocked(body + ". Failures: " + strings.Join(reasons, "; "))
}

// notifyLeadLocked sends role:lead a blocker from the hub, if there is a
// lead. Called with h.mu held.
func (h *Hub) notifyLeadLocked(body string) {
	_, err := h.mutateLocked(evSend, SendParams{From: hubSender, To: "role:lead", Kind: "blocker", Body: body})
	if err != nil && !errors.Is(err, bus.ErrNoRecipients) {
		fmt.Fprintf(os.Stderr, "tazzy hub: notify lead: %v\n", err)
	}
}

// escalated reports whether t's latest failure was the first on its n-th
// distinct agent.
func escalated(t graph.Task, n int) bool {
	if n <= 0 || len(t.Failures) == 0 {
		return false
	}
	distinct := map[string]bool{}
	for _, f := range t.Failures[:len(t.Failures)-1] {
		distinct[f.Agent] = true
	}
	last := t.Failures[len(t.Failures)-1].Agent
	return !distinct[last] && len(distinct)+1 == n
}
