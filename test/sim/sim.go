// Package sim is a swarm simulator: scripted fake agents drive a real hub
// over the wire (plan §11), while a monitor checks invariants on the hub's
// event stream. The task graph and each agent's behaviour come from a seed;
// goroutine interleaving does not, so runs differ in timing but every run
// must satisfy the same invariants.
//
// Chaos mode (plan §11) adds agents that die silently, stall past the stall
// timeout and come back, go quiet for less than that, drop their connection
// mid-request, or fail tasks for good.
package sim

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"tazzyronth/internal/config"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/lease"
	"tazzyronth/internal/proto"
	"tazzyronth/internal/store"
)

type Config struct {
	Agents   int
	Tasks    int
	Seed     uint64
	Files    int           // size of the shared file pool; smaller means more overlap
	MaxDeps  int           // per task, on earlier tasks
	Work     time.Duration // mean time an agent spends on a task (scaled by its speed)
	FailRate float64       // chance an attempt ends in a retryable fail

	// Chaos: chances per attempt, after the task's files are leased.
	KillRate  float64 // the agent dies silently (only the stall monitor can tell); a new agent replaces it
	StallRate float64 // the agent goes silent past stall + grace, then tries to carry on
	DelayRate float64 // the agent goes quiet for longer than stall but less than stall + grace
	DropRate  float64 // the connection drops right after the done request is sent
	FatalRate float64 // a non-retryable fail

	Stall       time.Duration // hub stall_s and stall_grace_s; default 5 minutes
	MaxAttempts int           // hub retries.max_attempts; default 3
}

type Report struct {
	Claims, LeaseConflicts, Failures int
	Kills, Stalls, Delays, Drops     int
	Removed                          int      // requests refused because the agent had been removed
	Done, Failed                     int      // final task states
	Violations                       []string // empty on a correct run
}

// Run starts a hub in dir, plans cfg.Tasks tasks and lets cfg.Agents fake
// agents work until every task is done or failed, or ctx ends.
func Run(ctx context.Context, dir string, cfg Config) (Report, error) {
	hc := config.Default()
	stall := cmp.Or(cfg.Stall, 5*time.Minute).Seconds()
	hc.Timeouts.Stall, hc.Timeouts.StallGrace = stall, stall
	if cfg.MaxAttempts > 0 {
		hc.Retries.MaxAttempts = cfg.MaxAttempts
	}
	h, err := hub.StartWith(dir, hub.Options{Config: &hc})
	if err != nil {
		return Report{}, err
	}
	defer h.Close()

	tasks := genTasks(cfg)
	mon := &monitor{root: dir, tasks: map[string]graph.Task{}, owner: map[string]string{},
		done: map[string]bool{}, held: map[string][]lease.Lease{}}
	for _, t := range tasks {
		mon.tasks[t.ID] = t
	}
	mc, err := hub.Connect(dir)
	if err != nil {
		return Report{}, err
	}
	defer mc.Close()
	events, err := mc.Subscribe(ctx)
	if err != nil {
		return Report{}, err
	}
	monDone := make(chan struct{})
	go func() {
		defer close(monDone)
		for ev := range events {
			mon.apply(ev)
		}
	}()

	if err := mc.Call(ctx, "task.add", hub.TaskAddParams{Tasks: tasks}, nil); err != nil {
		return Report{}, fmt.Errorf("plan: %w", err)
	}
	finished := make(chan struct{})
	go watchEnd(ctx, mc, finished)

	var (
		rep   Report
		repMu sync.Mutex
		ed    editors
		notes = &notebook{acked: map[string][]string{}}
		wg    sync.WaitGroup
		errs  = make(chan error, 1)
		n     uint64
	)
	var start func()
	start = func() { // one agent; a killed agent starts its replacement
		repMu.Lock()
		n++
		a := &agent{cfg: cfg, rng: rand.New(rand.NewPCG(cfg.Seed, n)), ed: &ed, notes: notes, finished: finished, dir: dir}
		repMu.Unlock()
		a.speed = 0.5 + 1.5*a.rng.Float64()
		if n%3 == 1 {
			a.caps = []string{"go"}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := a.run(ctx)
			repMu.Lock()
			r := &rep
			r.Claims += a.claims
			r.LeaseConflicts += a.conflicts
			r.Failures += a.failures
			r.Kills += a.kills
			r.Stalls += a.stalls
			r.Delays += a.delays
			r.Drops += a.drops
			r.Removed += a.removed
			repMu.Unlock()
			if errors.Is(err, errKilled) {
				start()
				return
			}
			if err != nil {
				select {
				case errs <- fmt.Errorf("agent %s: %w", a.id, err):
				default:
				}
			}
		}()
	}
	for range cfg.Agents {
		start()
	}
	wg.Wait()
	select {
	case err := <-errs:
		return rep, err
	default:
	}

	// Final hub state: every task done or failed, no lease left behind, no
	// acknowledged checkpoint lost.
	var snap hub.Snapshot
	if err := mc.Call(ctx, "status", nil, &snap); err != nil {
		return rep, err
	}
	mc.Close()
	<-monDone
	for _, t := range snap.Tasks {
		switch t.State {
		case graph.Done:
			rep.Done++
		case graph.Failed:
			rep.Failed++
		default:
			rep.Violations = append(rep.Violations, fmt.Sprintf("task %s ended %s", t.ID, t.State))
		}
		notes.check(t, "at the end")
	}
	mon.mu.Lock()
	rep.Violations = append(rep.Violations, mon.violations...)
	mon.mu.Unlock()
	rep.Violations = append(rep.Violations, ed.violations...)
	rep.Violations = append(rep.Violations, notes.violations...)
	if len(snap.Leases) > 0 {
		rep.Violations = append(rep.Violations, fmt.Sprintf("leases left: %v", snap.Leases))
	}
	return rep, nil
}

// watchEnd closes finished once every task is done or failed.
func watchEnd(ctx context.Context, c *hub.Client, finished chan struct{}) {
	defer close(finished)
	for ctx.Err() == nil {
		var ts []graph.Task
		if c.Call(ctx, "task.list", hub.TaskListParams{}, &ts) != nil {
			return
		}
		if !slices.ContainsFunc(ts, func(t graph.Task) bool { return t.State != graph.Done && t.State != graph.Failed }) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// genTasks builds a random DAG whose tasks declare overlapping files: plain
// paths in a few packages plus some whole-package globs.
func genTasks(cfg Config) []graph.Task {
	rng := rand.New(rand.NewPCG(cfg.Seed, 0))
	pool := make([]string, cfg.Files)
	for k := range pool {
		pool[k] = fmt.Sprintf("pkg%d/f%d.go", k%5, k)
		if k%4 == 3 {
			pool[k] = fmt.Sprintf("pkg%d/**", k%5)
		}
	}
	tasks := make([]graph.Task, cfg.Tasks)
	for i := range tasks {
		t := graph.Task{ID: fmt.Sprintf("t%02d", i), Priority: rng.IntN(3)}
		for range rng.IntN(cfg.MaxDeps + 1) {
			if i > 0 {
				if d := fmt.Sprintf("t%02d", rng.IntN(i)); !slices.Contains(t.Deps, d) {
					t.Deps = append(t.Deps, d)
				}
			}
		}
		for range 1 + rng.IntN(2) {
			if f := pool[rng.IntN(len(pool))]; !slices.Contains(t.Files, f) {
				t.Files = append(t.Files, f)
			}
		}
		if i%7 == 0 {
			t.Caps = []string{"go"}
		}
		tasks[i] = t
	}
	return tasks
}

var (
	errKilled  = errors.New("killed")
	errRemoved = errors.New("removed by the hub")
)

type agent struct {
	cfg      Config
	rng      *rand.Rand
	speed    float64
	caps     []string
	ed       *editors
	notes    *notebook
	finished <-chan struct{}
	dir      string

	c                           *hub.Client
	id                          string
	claims, conflicts, failures int
	kills, stalls, delays       int
	drops, removed              int
}

func (a *agent) pause(d time.Duration) bool {
	select {
	case <-a.finished:
		return false
	case <-time.After(d):
		return true
	}
}

// call makes an RPC, turning "you were removed" into errRemoved.
func (a *agent) call(ctx context.Context, method string, params, result any) error {
	err := a.c.Call(ctx, method, params, result)
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code == hub.CodeRemoved {
		a.removed++
		return errRemoved
	}
	return err
}

func (a *agent) join(ctx context.Context) error {
	var me hub.Agent
	if err := a.call(ctx, "agent.join", hub.JoinParams{Caps: a.caps}, &me); err != nil {
		return err
	}
	a.id = me.ID
	return nil
}

func (a *agent) run(ctx context.Context) error {
	var err error
	if a.c, err = hub.Connect(a.dir); err != nil {
		return err
	}
	defer func() { a.c.Close() }()
	if err := a.join(ctx); err != nil {
		return err
	}
	for {
		var t *graph.Task
		if err := a.call(ctx, "sched.next", hub.AgentParams{Agent: a.id}, &t); err != nil {
			return err
		}
		if t == nil {
			if !a.pause(time.Millisecond) {
				return nil
			}
			continue
		}
		a.claims++
		a.notes.check(*t, "on claim by "+a.id)
		err := a.attempt(ctx, *t)
		if errors.Is(err, errRemoved) {
			// The hub gave our task away: start over as a new agent, as the
			// removal message says.
			if err := a.join(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
	}
}

// attempt works on a claimed task until it is done, failed, or the agent
// dies or is removed.
func (a *agent) attempt(ctx context.Context, t graph.Task) error {
	// Lease all declared files at once (no hold-and-wait, so no deadlock).
	for len(t.Files) > 0 {
		err := a.call(ctx, "lease.acquire", hub.LeaseParams{Agent: a.id, Paths: t.Files}, nil)
		var pe *proto.Error
		if err == nil {
			break
		}
		if !errors.As(err, &pe) || pe.Code != hub.CodeConflict {
			return err
		}
		a.conflicts++
		time.Sleep(time.Duration(1+a.rng.IntN(2)) * time.Millisecond)
	}
	if err := a.note(ctx, t.ID, "leased"); err != nil {
		return err
	}

	stall := cmp.Or(a.cfg.Stall, 5*time.Minute)
	r := a.rng.Float64()
	switch {
	case r < a.cfg.KillRate:
		a.kills++
		return errKilled // the connection closes; the hub only sees silence
	case r < a.cfg.KillRate+a.cfg.StallRate:
		a.stalls++
		time.Sleep(3*stall + stall/2)                     // well past stall + grace: removed meanwhile
		if err := a.note(ctx, t.ID, "back"); err != nil { // refused, unless the hub was slower still
			return err
		}
	case r < a.cfg.KillRate+a.cfg.StallRate+a.cfg.DelayRate:
		a.delays++
		time.Sleep(stall + stall/4) // pinged, but back before the grace period ends
	}

	a.ed.begin(a.id, t.Files)
	time.Sleep(time.Duration(float64(a.cfg.Work) * a.speed * (0.5 + a.rng.Float64())))
	a.ed.end(a.id)
	if err := a.note(ctx, t.ID, "worked"); err != nil {
		return err
	}

	r = a.rng.Float64()
	switch {
	case r < a.cfg.FatalRate:
		a.failures++
		return a.call(ctx, "task.fail", hub.TaskFailParams{ID: t.ID, Agent: a.id, Reason: "fatal"}, nil)
	case r < a.cfg.FatalRate+a.cfg.FailRate:
		a.failures++
		return a.call(ctx, "task.fail", hub.TaskFailParams{ID: t.ID, Agent: a.id, Reason: "flaky", Retryable: true}, nil)
	}
	done := hub.TaskStateParams{ID: t.ID, State: graph.Done, Agent: a.id, Result: &graph.Result{Summary: "ok", Files: t.Files}}
	if a.rng.Float64() < a.cfg.DropRate {
		a.drops++
		// Send done, drop the connection before the reply, reconnect and
		// find out whether it landed.
		gone, cancel := context.WithCancel(ctx)
		cancel()
		a.c.Call(gone, "task.state", done, nil)
		a.c.Close()
		var err error
		if a.c, err = hub.Connect(a.dir); err != nil {
			return err
		}
		if a.doneByMe(ctx, t.ID) {
			return nil
		}
	}
	err := a.call(ctx, "task.state", done, nil)
	var pe *proto.Error
	if errors.As(err, &pe) && a.doneByMe(ctx, t.ID) {
		return nil // the dropped request landed after all, just later
	}
	return err
}

func (a *agent) doneByMe(ctx context.Context, task string) bool {
	var t graph.Task
	return a.call(ctx, "task.get", hub.IDParams{ID: task}, &t) == nil && t.State == graph.Done && t.Owner == a.id
}

// note writes a checkpoint and, once the hub has acknowledged it, records it
// as one that must never be lost.
func (a *agent) note(ctx context.Context, task, what string) error {
	text := fmt.Sprintf("%s %s %d", a.id, what, a.rng.Uint32())
	if err := a.call(ctx, "task.note", hub.TaskNoteParams{ID: task, Agent: a.id, Text: text}, nil); err != nil {
		return err
	}
	a.notes.ack(task, text)
	return nil
}

// notebook holds every checkpoint the hub acknowledged, to check none is
// lost when a task changes hands.
type notebook struct {
	mu         sync.Mutex
	acked      map[string][]string // task -> note texts
	violations []string
}

func (n *notebook) ack(task, text string) {
	n.mu.Lock()
	n.acked[task] = append(n.acked[task], text)
	n.mu.Unlock()
}

func (n *notebook) check(t graph.Task, when string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, want := range n.acked[t.ID] {
		if !slices.ContainsFunc(t.Checkpoints, func(c graph.Note) bool { return c.Text == want }) {
			n.violations = append(n.violations, fmt.Sprintf("task %s %s: checkpoint %q lost", t.ID, when, want))
		}
	}
}

// editors tracks which files each agent is "editing" right now, independent
// of the hub: overlapping edits mean the hub granted overlapping leases.
type editors struct {
	mu         sync.Mutex
	files      map[string][]string
	violations []string
}

func (e *editors) begin(agent string, files []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.files == nil {
		e.files = map[string][]string{}
	}
	for other, theirs := range e.files {
		for _, f := range files {
			for _, g := range theirs {
				if lease.Overlap(f, g) {
					e.violations = append(e.violations, fmt.Sprintf("%s edits %s while %s edits %s", agent, f, other, g))
				}
			}
		}
	}
	e.files[agent] = files
}

func (e *editors) end(agent string) {
	e.mu.Lock()
	delete(e.files, agent)
	e.mu.Unlock()
}

// monitor mirrors leases and task owners from the event stream and records
// any invariant violation: overlapping write leases, a task claimed before
// all its deps were done, or a task completed or failed by an agent that
// doesn't own it.
type monitor struct {
	root  string
	tasks map[string]graph.Task
	owner map[string]string
	done  map[string]bool

	mu         sync.Mutex
	held       map[string][]lease.Lease // holder -> leases
	violations []string
}

func (m *monitor) fail(format string, args ...any) {
	m.mu.Lock()
	m.violations = append(m.violations, fmt.Sprintf(format, args...))
	m.mu.Unlock()
}

// endTask records that task's current attempt ended: the hub releases the
// owner's leases with it.
func (m *monitor) endTask(task string) {
	m.mu.Lock()
	delete(m.held, m.owner[task])
	m.mu.Unlock()
	delete(m.owner, task)
}

func (m *monitor) apply(ev store.Event) {
	switch ev.Type {
	case "task.state":
		var p hub.TaskStateParams
		json.Unmarshal(ev.Data, &p)
		switch p.State {
		case graph.Claimed:
			for _, d := range m.tasks[p.ID].Deps {
				if !m.done[d] {
					m.fail("%s claimed %s before dep %s was done", p.Agent, p.ID, d)
				}
			}
			m.owner[p.ID] = p.Agent
		case graph.Done, graph.Failed, graph.Ready, graph.Cancelled:
			if p.Agent != m.owner[p.ID] {
				m.fail("seq %d: %s moved %s to %s while %q owns it", ev.Seq, p.Agent, p.ID, p.State, m.owner[p.ID])
			}
			m.endTask(p.ID)
			if p.State == graph.Done {
				m.done[p.ID] = true
			}
		}
	case "task.fail":
		var p hub.TaskFailParams
		json.Unmarshal(ev.Data, &p)
		if p.Agent != m.owner[p.ID] {
			m.fail("seq %d: %s failed %s while %q owns it", ev.Seq, p.Agent, p.ID, m.owner[p.ID])
		}
		m.endTask(p.ID)
	case "agent.leave":
		var p hub.LeaveParams
		json.Unmarshal(ev.Data, &p)
		for task, owner := range m.owner {
			if owner == p.Agent {
				m.endTask(task)
			}
		}
		m.mu.Lock()
		delete(m.held, p.Agent)
		m.mu.Unlock()
	case "lease.acquire":
		var p hub.LeaseParams
		json.Unmarshal(ev.Data, &p)
		mode := p.Mode
		if mode == "" {
			mode = lease.Write
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, raw := range p.Paths {
			path, _ := lease.Normalize(m.root, raw)
			for holder, ls := range m.held {
				for _, l := range ls {
					if holder != p.Agent && (mode == lease.Write || l.Mode == lease.Write) && lease.Overlap(path, l.Path) {
						m.violations = append(m.violations, fmt.Sprintf("seq %d: %s got %s %s while %s holds %s %s", ev.Seq, p.Agent, mode, path, holder, l.Mode, l.Path))
					}
				}
			}
			m.held[p.Agent] = append(m.held[p.Agent], lease.Lease{Path: path, Mode: mode, Holder: p.Agent})
		}
	case "lease.release":
		var p hub.LeaseReleaseParams
		json.Unmarshal(ev.Data, &p)
		m.mu.Lock()
		defer m.mu.Unlock()
		if len(p.Paths) == 0 {
			delete(m.held, p.Agent)
			return
		}
		m.held[p.Agent] = slices.DeleteFunc(m.held[p.Agent], func(l lease.Lease) bool {
			return slices.ContainsFunc(p.Paths, func(raw string) bool {
				n, _ := lease.Normalize(m.root, raw)
				return n == l.Path
			})
		})
	}
}
