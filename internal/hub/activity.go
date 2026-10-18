package hub

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tazzyronth/internal/activity"
	"tazzyronth/internal/config"
	"tazzyronth/internal/diff"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/store"
)

// evActivity is the type of the (unlogged) event that carries an
// activity.Activity to subscribers that asked for activity.
const evActivity = "activity"

// alertAgent owns activity that belongs to no agent (unattributed edits).
const alertAgent = "hub"

// Params of the activity RPCs.
type (
	ActivityListParams struct {
		Agent string `json:"agent,omitempty"` // empty: merged across agents
		After int64  `json:"after,omitempty"` // only events with a larger id
		Limit int    `json:"limit,omitempty"` // most recent N; default 100
	}
	ActivityDetailParams struct {
		ID int64 `json:"id"`
	}
	// HookParams carry a host hook payload (plan §3.9 source 2). Agent, if
	// set, is the Tazzyronth id of whoever ran the hook (TAZZY_AGENT_ID).
	HookParams struct {
		Agent   string          `json:"agent,omitempty"`
		Payload json.RawMessage `json:"payload"`
	}
	HookResult struct {
		Agent    string `json:"agent,omitempty"` // who it was attributed to
		Recorded bool   `json:"recorded"`
	}
	EventsParams struct {
		Activity bool `json:"activity,omitempty"` // also stream "activity" events
	}
)

// recordLocked stores an activity and streams it to activity subscribers.
// Task defaults to the agent's active task; absolute file paths inside the
// project are made relative. Activity from the agent itself (its stream or
// hooks) is a heartbeat. Called with h.mu held.
func (h *Hub) recordLocked(a activity.Activity) {
	if h.act == nil || a.Kind == "" || a.Agent == "" {
		return
	}
	if a.Task == "" {
		for _, t := range h.st.graph.List() {
			if t.Owner == a.Agent && active(t.State) {
				a.Task = t.ID
				break
			}
		}
	}
	for i, f := range a.Files {
		if filepath.IsAbs(f) {
			if rel, err := filepath.Rel(h.st.root, f); err == nil && !strings.HasPrefix(rel, "..") {
				f = rel
			}
		}
		a.Files[i] = filepath.ToSlash(f)
	}
	if (a.Source == activity.SrcStream || a.Source == activity.SrcHook) && h.st.agents[a.Agent] != nil {
		h.seen[a.Agent] = time.Now()
		delete(h.pinged, a.Agent)
	}
	a, err := h.act.Add(a)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tazzy hub: record activity: %v\n", err)
		return
	}
	data, _ := json.Marshal(a)
	h.publishLocked(store.Event{Time: a.TS, Type: evActivity, Data: data}, true)
}

func (h *Hub) record(a activity.Activity) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recordLocked(a)
}

// afterLocked turns a logged state change into activity (coordination calls
// are the hub's own source of truth) and keeps the diff watcher on the
// current leases. Called with h.mu held, after ev was applied.
func (h *Hub) afterLocked(ev store.Event, res any) {
	a := activity.Activity{TS: ev.Time, Source: activity.SrcHub}
	switch ev.Type {
	case evTaskNote:
		var p TaskNoteParams
		json.Unmarshal(ev.Data, &p)
		a.Agent, a.Task, a.Kind, a.Summary, a.Files = p.Agent, p.ID, activity.Intent, p.Text, p.Files
	case evSend:
		var p SendParams
		json.Unmarshal(ev.Data, &p)
		if p.From != hubSender {
			a.Agent, a.Kind, a.Summary = p.From, activity.Msg, fmt.Sprintf("→ %s %q", p.To, p.Body)
			a.Detail = ev.Data
		}
	case evLease:
		var p LeaseParams
		json.Unmarshal(ev.Data, &p)
		a.Agent, a.Kind, a.Files = p.Agent, activity.Lease, p.Paths
		a.Summary = fmt.Sprintf("lease %s %s", cmp.Or(string(p.Mode), "write"), strings.Join(p.Paths, " "))
	case evRelease:
		var p LeaseReleaseParams
		json.Unmarshal(ev.Data, &p)
		a.Agent, a.Kind, a.Files = p.Agent, activity.Lease, p.Paths
		a.Summary = strings.TrimSpace("release " + cmp.Or(strings.Join(p.Paths, " "), "all") + " " + p.Reason)
	case evTaskState:
		var p TaskStateParams
		json.Unmarshal(ev.Data, &p)
		r := res.(TaskStateResult)
		a.Agent, a.Task, a.Kind = cmp.Or(p.Agent, r.Owner), p.ID, activity.State
		a.Summary = fmt.Sprintf("task %s %s", p.ID, p.State)
		if p.Result != nil && p.Result.Summary != "" {
			a.Summary += ": " + p.Result.Summary
		}
	case evTaskFail:
		var p TaskFailParams
		json.Unmarshal(ev.Data, &p)
		r := res.(FailResult)
		a.Agent, a.Task, a.Kind = p.Agent, p.ID, activity.State
		a.Summary = fmt.Sprintf("task %s → %s: %s", p.ID, r.Task.State, p.Reason)
	case evLeave:
		var p LeaveParams
		json.Unmarshal(ev.Data, &p)
		a.Agent, a.Kind, a.Summary = p.Agent, activity.State, "left: "+cmp.Or(p.Reason, "left")
	}
	switch ev.Type {
	case evLease, evRelease, evTaskState, evTaskFail, evLeave:
		// Edits caught as a lease ends (with its task) are recorded first,
		// and belong to the task that just ended.
		h.editsLocked(h.watch.Sync(h.st.leases.List()), a.Agent, a.Task)
	}
	h.recordLocked(a)
}

// editsLocked records the watcher's changes; those by agent default to task.
func (h *Hub) editsLocked(changes []diff.Change, agent, task string) {
	for _, c := range changes {
		d, _ := json.Marshal(map[string]any{"path": c.Path, "diff": c.Diff, "added": c.Added, "removed": c.Removed})
		e := activity.Activity{
			Agent: c.Holder, Kind: activity.Edit, Source: activity.SrcDiff, Files: []string{c.Path}, Detail: d,
			Summary: fmt.Sprintf("%s +%d −%d", c.Path, c.Added, c.Removed),
		}
		if c.Holder == agent {
			e.Task = task
		}
		h.recordLocked(e)
	}
}

// activityLoop polls leased files for edits and, in a git repo, sweeps git
// status for edits to files nobody leased.
func (h *Hub) activityLoop() {
	defer h.wg.Done()
	poll := time.NewTicker(config.Seconds(h.cfg.Activity.Poll))
	defer poll.Stop()
	var sweep <-chan time.Time
	prefix, isGit := "", false
	if h.cfg.Activity.Sweep > 0 {
		if prefix, isGit = diff.GitPrefix(h.st.root); isGit {
			t := time.NewTicker(config.Seconds(h.cfg.Activity.Sweep))
			defer t.Stop()
			sweep = t.C
			h.sweep(prefix) // baseline
		}
	}
	for {
		select {
		case <-h.stop:
			return
		case <-poll.C:
			h.mu.Lock()
			h.editsLocked(h.watch.Poll(), "", "")
			h.mu.Unlock()
		case <-sweep:
			h.sweep(prefix)
		}
	}
}

func (h *Hub) sweep(prefix string) {
	paths, err := diff.GitStatus(h.st.root, prefix) // outside the lock: git can be slow
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.watch.Unattributed(paths, h.st.leases.List()) {
		h.recordLocked(activity.Activity{Agent: alertAgent, Kind: activity.Alert, Source: activity.SrcDiff, Files: []string{p},
			Summary: "⚠ unattributed edit: " + p + " changed but nobody leased it"})
	}
}

// streamTo is the supervisor's stdout hook: each worker's stream is decoded
// on its own goroutine into activity attributed to the worker.
func (h *Hub) streamTo(agent, format string) io.WriteCloser {
	pr, pw := io.Pipe()
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		activity.Decode(format, pr, func(a activity.Activity) {
			a.Agent, a.Source = agent, activity.SrcStream
			h.record(a)
		})
	}()
	return pw
}

func activityList(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
	p, err := decodeParams[ActivityListParams](raw)
	if err != nil {
		return nil, err
	}
	if p.Limit <= 0 {
		p.Limit = 100
	}
	return h.act.Recent(p.Agent, p.After, p.Limit), nil
}

func activityDetail(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
	p, err := decodeParams[ActivityDetailParams](raw)
	if err != nil {
		return nil, err
	}
	d, err := h.act.Detail(p.ID)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// activityHook records a host hook payload. Identity binding (plan §3.9):
// the payload's host identity (subagent agent_id, else session_id) is bound
// to a Tazzyronth agent whenever the payload names one (a tazzy tool call
// carrying "agent", or tazzy_join's/tazzy_start's result) or the hook was
// run with an explicit agent. Unbound identities are recorded as
// "host-<id prefix>". Bindings live in memory only; the next tazzy call
// after a hub restart re-binds.
func activityHook(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
	p, err := decodeParams[HookParams](raw)
	if err != nil {
		return nil, err
	}
	hk := activity.FromHook(p.Payload)
	h.mu.Lock()
	defer h.mu.Unlock()
	agent := cmp.Or(p.Agent, hk.Agent)
	if agent != "" && hk.Key != "" {
		h.hookIDs[hk.Key] = agent
	}
	if agent == "" {
		agent = h.hookIDs[hk.Key]
	}
	if agent == "" && hk.Key != "" {
		agent = "host-" + hk.Key[:min(8, len(hk.Key))]
	}
	if agent == "" || hk.Activity.Kind == "" {
		return HookResult{Agent: agent}, nil
	}
	if a := h.st.agents[agent]; a != nil && h.cfg.Adapters[a.Adapter].Stream != "" {
		// A hub-spawned worker whose stream is decoded already: just a heartbeat.
		h.seen[agent] = time.Now()
		delete(h.pinged, agent)
		return HookResult{Agent: agent}, nil
	}
	hk.Activity.Agent = agent
	h.recordLocked(hk.Activity)
	return HookResult{Agent: agent, Recorded: true}, nil
}

// status is the full snapshot plus the usage tally.
func status(h *Hub, _ *conn, _ json.RawMessage) (any, error) {
	h.mu.Lock()
	snap := h.st.snapshot()
	h.mu.Unlock()
	t := h.act.Totals()
	snap.Usage = &t
	return snap, nil
}

// taskState applies a task.state change (in worktree isolation, merging a
// finished task first) and tells the lead if cancelling the task failed its
// dependents.
func taskState(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
	p, err := decodeParams[TaskStateParams](raw)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	before, _ := h.st.graph.Get(p.ID)
	if p.State == graph.Done {
		if err := h.mergeLocked(p); err != nil {
			return nil, err
		}
	}
	res, err := h.mutateLocked(evTaskState, p)
	if err != nil {
		return nil, err
	}
	if active(before.State) && (p.State == graph.Ready || p.State == graph.Failed || p.State == graph.Cancelled) {
		h.abandonLocked(before.Owner, before)
	}
	if r := res.(TaskStateResult); r.State == graph.Cancelled && len(r.Cascaded) > 0 {
		h.notifyLeadLocked(fmt.Sprintf("task %s was cancelled; its dependents failed with it: %s", r.ID, strings.Join(r.Cascaded, ", ")))
	}
	return res, nil
}
