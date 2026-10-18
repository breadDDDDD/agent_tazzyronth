package hub

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"tazzyronth/internal/activity"
	"tazzyronth/internal/board"
	"tazzyronth/internal/bus"
	"tazzyronth/internal/config"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/lease"
	"tazzyronth/internal/proto"
	"tazzyronth/internal/sched"
	"tazzyronth/internal/store"
	"tazzyronth/internal/worktree"
)

// DefaultLeaseTTL is how long (seconds) a lease survives without any
// activity from its holder.
const DefaultLeaseTTL = 300

// Agent is a registered swarm member.
type Agent struct {
	ID      string    `json:"id"`
	Name    string    `json:"name,omitempty"`
	Role    string    `json:"role"`
	Caps    []string  `json:"caps,omitempty"`
	Adapter string    `json:"adapter,omitempty"` // set if the hub spawned it
	Joined  time.Time `json:"joined"`
}

// Params of the mutating RPC methods. Each is also the Data of the event of
// the same name in the log.
type (
	JoinParams struct {
		ID   string   `json:"id,omitempty"` // omit to have one assigned
		Name string   `json:"name,omitempty"`
		Role string   `json:"role,omitempty"` // default "worker"
		Caps []string `json:"caps,omitempty"`
		// Adapter marks a hub-spawned worker, pre-registered before its process
		// starts. A later join with the same id keeps it.
		Adapter string `json:"adapter,omitempty"`
	}
	// LeaveParams removes an agent: its leases are released and its active
	// tasks go back to ready.
	LeaveParams struct {
		Agent  string `json:"agent"`
		Reason string `json:"reason,omitempty"` // e.g. "retired", "exited: exit status 1", "stalled"
		// MaxAttempts is the retry limit in force (filled in by the hub; 0 = no
		// limit): each of the agent's tasks counts as a failed attempt.
		MaxAttempts int `json:"max_attempts,omitempty"`
	}
	// SwarmParams are the lead's settings from tazzy_start; empty fields mean
	// the config default.
	SwarmParams struct {
		Spawn     string `json:"spawn,omitempty"` // config.SpawnHub, SpawnHost or SpawnAuto
		MaxAgents int    `json:"max_agents,omitempty"`
		Adapter   string `json:"adapter,omitempty"`
		Client    string `json:"client,omitempty"`    // the lead's MCP client name; resolves spawn mode auto
		Isolation string `json:"isolation,omitempty"` // config.IsolationLease or IsolationWorktree
		// Integration is the branch finished tasks are merged into in worktree
		// isolation, set by the hub (kept by later swarm.set calls).
		Integration string `json:"integration,omitempty"`
	}
	TaskAddParams struct {
		Tasks []graph.Task `json:"tasks"`
		// Blocks are pending tasks that also wait for the new ones (which must
		// have no deps); the hub uses it for merge-conflict tasks.
		Blocks []string `json:"blocks,omitempty"`
	}
	TaskStateParams struct {
		ID     string        `json:"id"`
		State  graph.State   `json:"state"`
		Agent  string        `json:"agent,omitempty"` // required for claimed; otherwise must be the owner if set
		Result *graph.Result `json:"result,omitempty"`
	}
	TaskNoteParams struct {
		ID    string   `json:"id"`
		Agent string   `json:"agent,omitempty"`
		Text  string   `json:"text"`
		Files []string `json:"files,omitempty"` // touched so far
	}
	// TaskFailParams end the owner's attempt at a task (plan §3.7): it goes
	// back to ready if Retryable and attempts remain, else it fails for good
	// and so do its pending dependents.
	TaskFailParams struct {
		ID          string `json:"id"`
		Agent       string `json:"agent,omitempty"` // must be the owner if set
		Reason      string `json:"reason"`
		Retryable   bool   `json:"retryable,omitempty"`
		MaxAttempts int    `json:"max_attempts,omitempty"` // filled in by the hub, as for LeaveParams
	}
	SendParams struct {
		ID      string `json:"id,omitempty"` // client-chosen id makes retries idempotent
		From    string `json:"from,omitempty"`
		To      string `json:"to"`
		Kind    string `json:"kind,omitempty"`
		Body    string `json:"body"`
		ReplyTo string `json:"reply_to,omitempty"`
	}
	AckParams struct {
		Agent string   `json:"agent"`
		IDs   []string `json:"ids"`
	}
	SubscribeParams struct {
		Agent string `json:"agent"`
		Topic string `json:"topic"`
	}
	BoardSetParams struct {
		Key   string `json:"key"`
		Value string `json:"value"`
		By    string `json:"by,omitempty"`
	}
	DecideParams struct {
		Text string `json:"text"`
		By   string `json:"by,omitempty"`
	}
	LeaseParams struct {
		Agent string     `json:"agent"`
		Paths []string   `json:"paths"`          // paths or globs, relative to the project
		Mode  lease.Mode `json:"mode,omitempty"` // default write
		TTL   float64    `json:"ttl,omitempty"`  // seconds; default DefaultLeaseTTL
	}
	LeaseReleaseParams struct {
		Agent  string   `json:"agent"`
		Paths  []string `json:"paths,omitempty"`  // exact patterns; empty = all
		Reason string   `json:"reason,omitempty"` // "expired" when the hub let it lapse
	}
)

// Event types, one per mutating RPC method.
const (
	evJoin      = "agent.join"
	evLeave     = "agent.leave"
	evTaskAdd   = "task.add"
	evTaskState = "task.state"
	evTaskNote  = "task.note"
	evTaskFail  = "task.fail"
	evSend      = "msg.send"
	evAck       = "msg.ack"
	evSubscribe = "msg.subscribe"
	evBoardSet  = "board.set"
	evDecide    = "board.decide"
	evLease     = "lease.acquire"
	evRelease   = "lease.release"
	evSwarm     = "swarm.set"
)

// state is everything the hub knows. It changes only through apply, which is
// deterministic in the event (time comes from the event, ids from counters),
// so replaying the log rebuilds it exactly.
type state struct {
	root     string // project dir; lease paths are relative to it
	agents   map[string]*Agent
	left     map[string]string // agents that left -> why; their ids are not handed out again
	graph    *graph.Graph
	bus      *bus.Bus
	board    *board.Board
	leases   *lease.Table
	failedBy map[string][]string // task id -> agents that failed or gave it up
	swarm    SwarmParams
	spawned  int // hub-spawned workers ever registered, for the spawn budget
}

func newState(root string) *state {
	g := graph.New()
	return &state{
		root:     root,
		agents:   map[string]*Agent{},
		left:     map[string]string{},
		graph:    g,
		bus:      bus.New(g.Owner),
		board:    board.New(),
		leases:   lease.New(),
		failedBy: map[string][]string{},
	}
}

func decode[T any](data json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return v, fmt.Errorf("bad params: %w", err)
	}
	return v, nil
}

// apply validates and applies one event. It either fully applies or returns
// an error with state unchanged. The result is what the RPC returns.
func (s *state) apply(ev store.Event) (any, error) {
	switch ev.Type {
	case evJoin:
		p, err := decode[JoinParams](ev.Data)
		if err != nil {
			return nil, err
		}
		return s.join(p, ev.Time), nil

	case evSwarm:
		p, err := decode[SwarmParams](ev.Data)
		if err != nil {
			return nil, err
		}
		if p.Spawn != "" && p.Spawn != config.SpawnHub && p.Spawn != config.SpawnHost && p.Spawn != config.SpawnAuto {
			return nil, fmt.Errorf("spawn must be %q, %q or %q, got %q", config.SpawnHub, config.SpawnHost, config.SpawnAuto, p.Spawn)
		}
		if p.MaxAgents < 0 {
			return nil, errors.New("max_agents must not be negative")
		}
		if p.Isolation != "" && p.Isolation != config.IsolationLease && p.Isolation != config.IsolationWorktree {
			return nil, fmt.Errorf("isolation must be %q or %q, got %q", config.IsolationLease, config.IsolationWorktree, p.Isolation)
		}
		p.Integration = cmp.Or(p.Integration, s.swarm.Integration)
		s.swarm = p
		return p, nil

	case evLeave:
		p, err := decode[LeaveParams](ev.Data)
		if err != nil {
			return nil, err
		}
		if s.agents[p.Agent] == nil {
			return nil, fmt.Errorf("unknown agent %q", p.Agent)
		}
		res := LeaveResult{Released: s.leases.Release(p.Agent, nil), Tasks: []FailResult{}}
		reason := cmp.Or(p.Reason, "left")
		for _, t := range s.graph.List() {
			if t.Owner == p.Agent && active(t.State) {
				r, err := s.failTask(t.ID, p.Agent, "agent "+p.Agent+" removed: "+reason, true, p.MaxAttempts, ev.Time)
				if err == nil {
					res.Tasks = append(res.Tasks, r)
				}
			}
		}
		delete(s.agents, p.Agent)
		s.left[p.Agent] = reason
		s.bus.Leave(p.Agent)
		return res, nil

	case evTaskFail:
		p, err := decode[TaskFailParams](ev.Data)
		if err != nil {
			return nil, err
		}
		if p.Reason == "" {
			return nil, errors.New("reason is required")
		}
		return s.failTask(p.ID, p.Agent, p.Reason, p.Retryable, p.MaxAttempts, ev.Time)

	case evTaskAdd:
		p, err := decode[TaskAddParams](ev.Data)
		if err != nil {
			return nil, err
		}
		for i := range p.Tasks {
			if p.Tasks[i].Files, err = s.normalize(p.Tasks[i].Files); err != nil {
				return nil, fmt.Errorf("task %q: %w", p.Tasks[i].ID, err)
			}
		}
		for _, b := range p.Blocks {
			if t, ok := s.graph.Get(b); !ok || t.State != graph.Pending {
				return nil, fmt.Errorf("blocks: %q is not a pending task", b)
			}
		}
		var ids []string
		for _, t := range p.Tasks {
			if len(p.Blocks) > 0 && len(t.Deps) > 0 {
				return nil, fmt.Errorf("task %q: tasks that block others can't have deps", t.ID)
			}
			ids = append(ids, t.ID)
		}
		if err := s.graph.Add(p.Tasks...); err != nil {
			return nil, err
		}
		for _, b := range p.Blocks {
			s.graph.AddDeps(b, ids...) // can't fail: checked above, and the new tasks have no deps
		}
		out := make([]graph.Task, len(p.Tasks))
		for i, t := range p.Tasks {
			out[i], _ = s.graph.Get(t.ID)
		}
		return out, nil

	case evTaskState:
		p, err := decode[TaskStateParams](ev.Data)
		if err != nil {
			return nil, err
		}
		if p.State == graph.Claimed && s.agents[p.Agent] == nil {
			return nil, fmt.Errorf("unknown agent %q", p.Agent)
		}
		before, ok := s.graph.Get(p.ID)
		if !ok {
			return nil, fmt.Errorf("%w: %q", graph.ErrNotFound, p.ID)
		}
		if p.State != graph.Claimed {
			if err := checkOwner(before, p.Agent); err != nil {
				return nil, err
			}
		}
		if err := s.graph.SetState(p.ID, p.State, p.Agent); err != nil {
			return nil, err
		}
		if p.Result != nil {
			s.graph.SetResult(p.ID, p.Result) // can't fail: SetState found the task
		}
		if owner := before.Owner; owner != "" {
			switch p.State {
			case graph.Failed, graph.Ready:
				if !slices.Contains(s.failedBy[p.ID], owner) {
					s.failedBy[p.ID] = append(s.failedBy[p.ID], owner)
				}
				fallthrough
			case graph.Done, graph.Cancelled:
				// An agent works one task at a time, so its leases go with the task.
				s.leases.Release(owner, nil)
			}
		}
		res := TaskStateResult{}
		if p.State == graph.Cancelled {
			// Dependents could never run: fail them, as for a failed task.
			res.Cascaded = s.graph.FailDependents(p.ID)
			for _, c := range res.Cascaded {
				s.graph.AddFailure(c, graph.Note{Time: ev.Time, Text: "dependency " + p.ID + " was cancelled"})
			}
		}
		res.Task, _ = s.graph.Get(p.ID)
		return res, nil

	case evTaskNote:
		p, err := decode[TaskNoteParams](ev.Data)
		if err != nil {
			return nil, err
		}
		if t, ok := s.graph.Get(p.ID); ok {
			if err := checkOwner(t, p.Agent); err != nil {
				return nil, err
			}
		}
		if err := s.graph.AddNote(p.ID, graph.Note{Time: ev.Time, Agent: p.Agent, Text: p.Text, Files: p.Files}); err != nil {
			return nil, err
		}
		t, _ := s.graph.Get(p.ID)
		return t, nil

	case evSend:
		p, err := decode[SendParams](ev.Data)
		if err != nil {
			return nil, err
		}
		if task, ok := strings.CutPrefix(p.To, "task:"); ok {
			if _, exists := s.graph.Get(task); !exists {
				return nil, fmt.Errorf("%w: %q", graph.ErrNotFound, task)
			}
		}
		return s.bus.Send(bus.Message{ID: p.ID, From: p.From, To: p.To, Kind: p.Kind, Body: p.Body, ReplyTo: p.ReplyTo}, ev.Time)

	case evAck:
		p, err := decode[AckParams](ev.Data)
		if err != nil {
			return nil, err
		}
		return map[string]int{"acked": s.bus.Ack(p.Agent, p.IDs)}, nil

	case evSubscribe:
		p, err := decode[SubscribeParams](ev.Data)
		if err != nil {
			return nil, err
		}
		return struct{}{}, s.bus.Subscribe(p.Agent, p.Topic)

	case evBoardSet:
		p, err := decode[BoardSetParams](ev.Data)
		if err != nil {
			return nil, err
		}
		return s.board.Set(p.Key, p.Value, p.By, ev.Time)

	case evDecide:
		p, err := decode[DecideParams](ev.Data)
		if err != nil {
			return nil, err
		}
		return s.board.Decide(p.Text, p.By, ev.Time)

	case evLease:
		p, err := decode[LeaseParams](ev.Data)
		if err != nil {
			return nil, err
		}
		if s.agents[p.Agent] == nil {
			return nil, fmt.Errorf("unknown agent %q", p.Agent)
		}
		mode, err := checkMode(p.Mode)
		if err != nil {
			return nil, err
		}
		if p.TTL <= 0 {
			p.TTL = DefaultLeaseTTL
		}
		paths, err := s.normalizeFor(p.Agent, p.Paths)
		if err != nil {
			return nil, err
		}
		if len(paths) == 0 {
			return nil, errors.New("paths is required")
		}
		if c := s.leases.Acquire(p.Agent, paths, mode, p.TTL, ev.Time); c != nil {
			return nil, conflictError(c)
		}
		return map[string]any{"leases": s.leases.Held(p.Agent)}, nil

	case evRelease:
		p, err := decode[LeaseReleaseParams](ev.Data)
		if err != nil {
			return nil, err
		}
		paths, err := s.normalizeFor(p.Agent, p.Paths)
		if err != nil {
			return nil, err
		}
		released := s.leases.Release(p.Agent, paths)
		if released == nil {
			released = []lease.Lease{}
		}
		return map[string]any{"released": released}, nil
	}
	return nil, fmt.Errorf("unknown event type %q", ev.Type)
}

func active(st graph.State) bool {
	return st == graph.Claimed || st == graph.Running || st == graph.Blocked
}

// checkOwner rejects a change to a task by an agent other than its owner
// (including any agent while the task has none). Changes that name no agent
// (the lead cancelling, say) are allowed.
func checkOwner(t graph.Task, agent string) error {
	switch {
	case agent == "" || t.Owner == agent:
		return nil
	case t.Owner == "":
		return fmt.Errorf("task %q is %s and not owned by %s", t.ID, t.State, agent)
	}
	return fmt.Errorf("task %q is owned by %s, not %s", t.ID, t.Owner, agent)
}

// TaskStateResult is task.state's result: the task, plus the dependents that
// failed because it was cancelled.
type TaskStateResult struct {
	graph.Task
	Cascaded []string `json:"cascaded,omitempty"`
	Worktree string   `json:"worktree,omitempty"` // sched.next in worktree isolation: where to work
}

// FailResult is the outcome of a failed attempt: task.fail returns one, and
// agent.leave one per task the agent held.
type FailResult struct {
	Task     graph.Task `json:"task"`               // now ready (requeued) or failed
	Cascaded []string   `json:"cascaded,omitempty"` // dependents that failed with it
}

type LeaveResult struct {
	Released []lease.Lease `json:"released"`
	Tasks    []FailResult  `json:"tasks"`
}

// failTask ends the current attempt at task id (owned by agent, if set) with
// reason. The task is requeued if retry is set and attempts remain
// (maxAttempts, or the task's own limit; 0 = none), else it fails for good,
// together with every pending task that depends on it.
func (s *state) failTask(id, agent, reason string, retry bool, maxAttempts int, at time.Time) (FailResult, error) {
	t, ok := s.graph.Get(id)
	if !ok {
		return FailResult{}, fmt.Errorf("%w: %q", graph.ErrNotFound, id)
	}
	if err := checkOwner(t, agent); err != nil {
		return FailResult{}, err
	}
	if !active(t.State) {
		return FailResult{}, fmt.Errorf("task %q is %s, not in progress", id, t.State)
	}
	maxAttempts = cmp.Or(t.MaxAttempts, maxAttempts)
	to := graph.Failed
	if retry && (maxAttempts <= 0 || t.Attempts < maxAttempts) {
		to = graph.Ready
	}
	if err := s.graph.SetState(id, to, ""); err != nil {
		return FailResult{}, err
	}
	s.graph.AddFailure(id, graph.Note{Time: at, Agent: t.Owner, Text: reason})
	if !slices.Contains(s.failedBy[id], t.Owner) {
		s.failedBy[id] = append(s.failedBy[id], t.Owner)
	}
	s.leases.Release(t.Owner, nil)
	var r FailResult
	if to == graph.Failed {
		r.Cascaded = s.graph.FailDependents(id)
		for _, c := range r.Cascaded {
			s.graph.AddFailure(c, graph.Note{Time: at, Text: "dependency " + id + " failed"})
		}
	}
	r.Task, _ = s.graph.Get(id)
	return r, nil
}

func checkMode(m lease.Mode) (lease.Mode, error) {
	switch m {
	case "":
		return lease.Write, nil
	case lease.Read, lease.Write:
		return m, nil
	}
	return "", fmt.Errorf("mode must be read or write, got %q", m)
}

// normalizeFor is normalize for paths an agent names: absolute paths inside
// its worktree (worktree isolation) are taken relative to the worktree.
func (s *state) normalizeFor(agent string, paths []string) ([]string, error) {
	if agent == "" {
		return s.normalize(paths)
	}
	wt := worktree.Dir(s.root, agent)
	out := slices.Clone(paths)
	for i, p := range out {
		if filepath.IsAbs(p) {
			if n, err := lease.Normalize(wt, p); err == nil {
				out[i] = n
			}
		}
	}
	return s.normalize(out)
}

func (s *state) normalize(paths []string) ([]string, error) {
	if paths == nil {
		return nil, nil
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		var err error
		if out[i], err = lease.Normalize(s.root, p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func conflictError(c []lease.Conflict) *proto.Error {
	msgs := make([]string, len(c))
	for i, x := range c {
		msgs[i] = x.String()
	}
	data, _ := json.Marshal(c)
	return &proto.Error{Code: CodeConflict, Message: "lease conflict: " + strings.Join(msgs, "; "), Data: data}
}

// pick chooses the best ready task for agent (plan §3.2), or "" if none fits.
func (s *state) pick(agent string, w sched.Weights) (string, error) {
	a := s.agents[agent]
	if a == nil {
		return "", fmt.Errorf("unknown agent %q", agent)
	}
	me := sched.Agent{ID: a.ID, Caps: a.Caps}
	for _, l := range s.leases.Held(agent) {
		me.Files = append(me.Files, l.Path)
	}
	heights := s.graph.Heights()
	var cands []sched.Candidate
	for _, t := range s.graph.List() {
		switch {
		case t.State == graph.Ready:
			c := sched.Candidate{Task: t, Height: heights[t.ID], FailedBy: s.failedBy[t.ID]}
			for _, f := range t.Files {
				if s.leases.Conflicts(agent, []string{f}, lease.Write) != nil {
					c.Conflicts++
				}
			}
			cands = append(cands, c)
		case t.State == graph.Done && t.Owner == agent:
			me.Files = append(me.Files, t.Files...)
			if t.Result != nil {
				for _, f := range t.Result.Files {
					if n, err := lease.Normalize(s.root, f); err == nil {
						me.Files = append(me.Files, n)
					}
				}
			}
		}
	}
	if i := sched.Pick(w, cands, me); i >= 0 {
		return cands[i].Task.ID, nil
	}
	return "", nil
}

func (s *state) join(p JoinParams, t time.Time) Agent {
	if p.Role == "" {
		p.Role = "worker"
	}
	if p.ID == "" {
		p.ID = s.nextID()
	}
	delete(s.left, p.ID)
	a := s.agents[p.ID]
	if a == nil {
		a = &Agent{ID: p.ID, Joined: t, Adapter: p.Adapter}
		s.agents[p.ID] = a
		if p.Adapter != "" {
			s.spawned++
		}
	}
	// Re-joining with a known id updates the registration (a spawned worker's
	// tazzy_join lands here).
	a.Name, a.Role, a.Caps = p.Name, p.Role, slices.Clone(p.Caps)
	s.bus.Join(a.ID, a.Role)
	return *a
}

// nextID is the id the next join without one gets.
func (s *state) nextID() string {
	for n := len(s.agents) + 1; ; n++ {
		if id := fmt.Sprintf("a%d", n); s.agents[id] == nil && s.left[id] == "" {
			return id
		}
	}
}

func (s *state) agentList() []Agent {
	out := make([]Agent, 0, len(s.agents))
	for _, a := range s.agents {
		out = append(out, *a)
	}
	slices.SortFunc(out, func(x, y Agent) int { return strings.Compare(x.ID, y.ID) })
	return out
}

// Snapshot is a full read-only view of the state, returned by the "status"
// method and used to check that replay is deterministic.
type Snapshot struct {
	Agents       []Agent                  `json:"agents"`
	Tasks        []graph.Task             `json:"tasks"`
	CriticalPath []string                 `json:"critical_path"`
	Queues       map[string][]bus.Message `json:"queues"`
	Board        []board.Entry            `json:"board"`
	Decisions    []board.Decision         `json:"decisions"`
	Leases       []lease.Lease            `json:"leases"`
	Swarm        SwarmParams              `json:"swarm"`
	Usage        *activity.Totals         `json:"usage,omitempty"` // tokens and cost (status only)
}

func (s *state) snapshot() Snapshot {
	return Snapshot{
		Agents:       s.agentList(),
		Tasks:        s.graph.List(),
		CriticalPath: s.graph.CriticalPath(),
		Queues:       s.bus.Queues(),
		Board:        s.board.List(""),
		Decisions:    s.board.Decisions(),
		Leases:       s.leases.List(),
		Swarm:        s.swarm,
	}
}

// finished reports whether there is a plan and all of it has ended (done,
// failed or cancelled), so no worker has anything left to do or wait for.
func (s *state) finished() bool {
	tasks := s.graph.List()
	return len(tasks) > 0 && !slices.ContainsFunc(tasks, func(t graph.Task) bool {
		return t.State != graph.Done && t.State != graph.Failed && t.State != graph.Cancelled
	})
}

// width is how many ready tasks could run at once: a greedy pick, in id
// order, of ready tasks whose declared files overlap neither each other nor a
// current lease (plan §3.3). Tasks that declare no files never conflict.
func (s *state) width() int {
	var taken []string
	for _, l := range s.leases.List() {
		taken = append(taken, l.Path)
	}
	n := 0
	for _, t := range s.graph.List() {
		if t.State != graph.Ready {
			continue
		}
		clash := slices.ContainsFunc(t.Files, func(f string) bool {
			return slices.ContainsFunc(taken, func(g string) bool { return lease.Overlap(f, g) })
		})
		if !clash {
			taken = append(taken, t.Files...)
			n++
		}
	}
	return n
}
