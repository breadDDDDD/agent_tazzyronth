package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"tazzyronth/internal/board"
	"tazzyronth/internal/bus"
	"tazzyronth/internal/config"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/lease"
	"tazzyronth/internal/proto"
	"tazzyronth/playbooks"
)

const (
	maxWait     = 45 * time.Second // stays under typical MCP tool-call timeouts
	defaultWait = 30 * time.Second
	leadID      = "lead" // fixed so a restarted lead shim resumes the same identity
)

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	run         func(ctx context.Context, s *session, args json.RawMessage) (any, error)
	noInbox     bool // don't piggyback the default agent's inbox (tazzy_join: the caller may be someone else)
}

func findTool(name string) *tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

const taskSchema = `{"type":"object","properties":{
"id":{"type":"string"},"title":{"type":"string"},"spec":{"type":"string","description":"what to do and how to verify it"},
"deps":{"type":"array","items":{"type":"string"},"description":"ids of tasks that must be done first"},
"caps":{"type":"array","items":{"type":"string"},"description":"capabilities a worker needs"},
"files":{"type":"array","items":{"type":"string"},"description":"paths/globs it will edit"},
"priority":{"type":"integer","description":"higher runs first"}},"required":["id"]}`

func schema(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	if required == nil {
		req = []byte("[]")
	}
	s := `{"type":"object","properties":{` + props + `},"required":` + string(req) + `}`
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic("bad schema: " + err.Error() + ": " + s)
	}
	b, _ := json.Marshal(v) // compact
	return b
}

// agentSchema is schema plus the optional "agent" argument every tool used
// after registering accepts (several agents may share one shim).
func agentSchema(props string, required ...string) json.RawMessage {
	return schema(`"agent":{"type":"string","description":"your agent id from tazzy_join/tazzy_start; pass it if this server may be shared with other agents"},`+props, required...)
}

// tools is the MCP tool surface (plan §4).
var tools = []tool{
	{
		Name:        "tazzy_start",
		Description: "Start a swarm for a goal, register this session as the lead and open the dashboard. Returns the lead playbook: follow it. spawn: \"host\" (you start workers, e.g. your subagents, which call tazzy_join), \"hub\" (the hub launches and scales headless workers up to max_agents with adapter, e.g. claude, codex, gemini) or \"auto\" (host for clients with native subagents, else hub). isolation: \"lease\" (shared checkout, file leases) or \"worktree\" (each worker gets its own git worktree and branch; finished work is merged into an integration branch, never your checked-out branch; needs a git repo with a commit). Defaults come from .tazzyronth/config.json.",
		InputSchema: schema(`"goal":{"type":"string"},"spawn":{"type":"string","enum":["auto","host","hub"]},"max_agents":{"type":"integer"},"adapter":{"type":"string"},"isolation":{"type":"string","enum":["lease","worktree"]},"ui":{"type":"string","enum":["host"],"description":"set by host integrations that draw the dashboard themselves (the Claude Code plugin); leave unset"}`, "goal"),
		run:         start,
	},
	{
		Name:        "tazzy_plan",
		Description: "Add tasks to the swarm's DAG. deps may name existing tasks or others in the same batch.",
		InputSchema: agentSchema(`"tasks":{"type":"array","items":`+taskSchema+`}`, "tasks"),
		run:         plan,
	},
	{
		Name:        "tazzy_status",
		Description: "Snapshot of agents and tasks. verbose adds specs, checkpoints, results, queued messages and the board.",
		InputSchema: agentSchema(`"verbose":{"type":"boolean"}`),
		run:         status,
	},
	{
		Name:        "tazzy_control",
		Description: "Lead control. cancel: target = task id. spawn: start a hub-managed worker (target = adapter, optional). retire: remove an agent and stop its process (target = agent id); its task goes back to ready. pause: hand out no new tasks and spawn no workers (running tasks go on); resume: undo pause. stop: end the swarm for good: every unfinished task is cancelled and workers are told to exit. pause, resume and stop take no target and return the swarm's state.",
		InputSchema: agentSchema(`"action":{"type":"string","enum":["cancel","spawn","retire","pause","resume","stop"]},"target":{"type":"string"}`, "action"),
		run:         control,
	},
	{
		Name:        "tazzy_join",
		Description: "Register this session as a worker. Then loop: tazzy_next, do the task, tazzy_done or tazzy_fail.",
		InputSchema: schema(`"caps":{"type":"array","items":{"type":"string"}},"name":{"type":"string"}`),
		run:         join,
		noInbox:     true,
	},
	{
		Name:        "tazzy_next",
		Description: "Claim the best ready task for you (priority, critical path, your caps and files, lease conflicts). wait: seconds to block for one (max 45). Returns task null if none.",
		InputSchema: agentSchema(`"wait":{"type":"number"}`),
		run:         next,
	},
	{
		Name:        "tazzy_progress",
		Description: "Checkpoint your task: what is done so far and the files you touched (shown to whoever resumes it if you fail or stop). Also a heartbeat.",
		InputSchema: agentSchema(`"task":{"type":"string"},"note":{"type":"string"},"files_touched":{"type":"array","items":{"type":"string"}}`, "task", "note"),
		run:         progress,
	},
	{
		Name:        "tazzy_done",
		Description: "Complete your task and release your leases. summary and artifacts (name -> value, e.g. a path) are passed to dependent tasks.",
		InputSchema: agentSchema(`"task":{"type":"string"},"summary":{"type":"string"},"files":{"type":"array","items":{"type":"string"}},"artifacts":{"type":"object","additionalProperties":{"type":"string"}}`, "task", "summary"),
		run:         done,
	},
	{
		Name:        "tazzy_fail",
		Description: "Give up on your task. retryable puts it back in the ready queue for another agent (until its attempts run out); otherwise it fails for good and the lead is told. Checkpoint first so the next owner can resume.",
		InputSchema: agentSchema(`"task":{"type":"string"},"reason":{"type":"string"},"retryable":{"type":"boolean"}`, "task", "reason"),
		run:         fail,
	},
	{
		Name:        "tazzy_propose",
		Description: "Add subtasks or follow-ups discovered while working on parent.",
		InputSchema: agentSchema(`"parent":{"type":"string"},"tasks":{"type":"array","items":`+taskSchema+`}`, "parent", "tasks"),
		run:         propose,
	},
	{
		Name:        "tazzy_lease",
		Description: "Lease files before editing them. paths: files, directories or globs (src/api/**). mode write (default, exclusive) or read (shared). All or nothing; a conflict names the holder: tazzy_wait(until=\"lease:<path>\") or message them. release: drop these paths (or all if none). Leases end when your task does.",
		InputSchema: agentSchema(`"paths":{"type":"array","items":{"type":"string"}},"mode":{"type":"string","enum":["write","read"]},"release":{"type":"boolean"}`),
		run:         leaseOp,
	},
	{
		Name:        "tazzy_send",
		Description: "Send a message. to: agent:<id>, role:<name> (e.g. role:lead), task:<id> (its current owner), topic:<name> or broadcast.",
		InputSchema: agentSchema(`"to":{"type":"string"},"kind":{"type":"string","enum":["info","question","handoff","blocker","directive"]},"body":{"type":"string"},"reply_to":{"type":"string","description":"id of the message being answered"}`, "to", "body"),
		run:         send,
	},
	{
		Name:        "tazzy_wait",
		Description: "Block until a condition holds or timeout (seconds, max 45). until: task:<id> (task finished), msg (a message arrived), lease:<path> (nobody else leases it) or any (a message or any task change). Use instead of polling.",
		InputSchema: agentSchema(`"until":{"type":"string"},"timeout":{"type":"number"}`, "until"),
		run:         wait,
	},
	{
		Name:        "tazzy_board",
		Description: "Shared board. get/set a key, list keys under a prefix (key), or decide: append value to the decision log.",
		InputSchema: agentSchema(`"op":{"type":"string","enum":["get","set","list","decide"]},"key":{"type":"string"},"value":{"type":"string"}`, "op"),
		run:         boardOp,
	},
}

func decodeArgs[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("bad arguments: %w", err)
	}
	return v, nil
}

func waitDuration(secs float64, def time.Duration) time.Duration {
	d := time.Duration(secs * float64(time.Second))
	if secs == 0 {
		d = def
	}
	return min(max(d, 0), maxWait)
}

func start(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	p, err := decodeArgs[struct {
		Goal, Spawn, Adapter, UI, Isolation string
		MaxAgents                           int `json:"max_agents"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if p.Goal == "" {
		return nil, errors.New("goal is required")
	}
	var a hub.Agent
	if err := s.call(ctx, "agent.join", hub.JoinParams{ID: leadID, Role: "lead"}, &a); err != nil {
		return nil, err
	}
	s.setAgent(a.ID, true)
	// Workers idle since an earlier swarm would otherwise count against this one's max_agents.
	var pruned struct{ Released []string }
	if err := s.call(ctx, "agent.prune", nil, &pruned); err != nil {
		return nil, err
	}
	if err := s.call(ctx, "board.set", hub.BoardSetParams{Key: "goal", Value: p.Goal, By: a.ID}, nil); err != nil {
		return nil, err
	}
	s.mu.Lock()
	// ui "host": the host shows the dashboard itself (Claude Code's pane).
	client, launch := s.client, s.launch != nil && !s.launched && p.UI != "host"
	s.launched = s.launched || launch
	s.mu.Unlock()
	var sw hub.SwarmParams
	if err := s.call(ctx, "swarm.set", hub.SwarmParams{Spawn: p.Spawn, MaxAgents: p.MaxAgents, Adapter: p.Adapter, Client: client, Isolation: p.Isolation}, &sw); err != nil {
		return nil, err
	}
	out := map[string]any{"agent": a.ID, "spawn": sw.Spawn, "max_agents": sw.MaxAgents, "isolation": sw.Isolation}
	if len(pruned.Released) > 0 {
		out["released"] = pruned.Released // idle workers from an earlier swarm
	}
	if sw.Integration != "" {
		out["integration"] = sw.Integration
		out["merge"] = "finished tasks are merged into the integration branch, never into the checked-out branch: when the swarm is done, tell the user to review and merge it"
	}
	if sw.Spawn == config.SpawnHub {
		out["adapter"] = sw.Adapter
		out["next"] = "tazzy_plan the work; the hub starts workers as tasks become ready"
	} else {
		out["next"] = "tazzy_plan the work, then start workers with your subagent tool; each calls tazzy_join"
	}
	if launch {
		out["dashboard"] = s.launch()
	} else if p.UI == "host" {
		out["dashboard"] = "shown inside the host (Tazzyronth pane)"
	}
	out["playbook"] = playbooks.Lead
	return out, nil
}

func join(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	p, err := decodeArgs[struct {
		Caps []string
		Name string
	}](raw)
	if err != nil {
		return nil, err
	}
	var a hub.Agent
	// A hub-spawned worker's shim was started with its pre-registered id.
	if err := s.call(ctx, "agent.join", hub.JoinParams{ID: s.preset, Name: p.Name, Caps: p.Caps}, &a); err != nil {
		return nil, err
	}
	s.setAgent(a.ID, false)
	var b struct {
		Entries   []board.Entry    `json:"entries"`
		Decisions []board.Decision `json:"decisions"`
	}
	if err := s.call(ctx, "board.list", hub.PrefixParams{}, &b); err != nil {
		return nil, err
	}
	out := map[string]any{"agent": a.ID, "note": "pass agent=\"" + a.ID + "\" on every later tazzy_* call: this MCP server may be shared with other agents"}
	if s.preset == "" {
		out["playbook"] = playbooks.Worker // hub-spawned workers got it as their prompt
	}
	var keys []string
	for _, e := range b.Entries {
		if e.Key == "goal" {
			out["goal"] = e.Value
		} else {
			keys = append(keys, e.Key)
		}
	}
	if keys != nil {
		out["board_keys"] = keys
	}
	if len(b.Decisions) > 0 {
		texts := make([]string, len(b.Decisions))
		for i, d := range b.Decisions {
			texts[i] = d.Text
		}
		out["decisions"] = texts
	}
	return out, nil
}

func addTasks(ctx context.Context, s *session, tasks []graph.Task) (any, error) {
	if len(tasks) == 0 {
		return nil, errors.New("tasks is required")
	}
	var added []graph.Task
	if err := s.call(ctx, "task.add", hub.TaskAddParams{Tasks: tasks}, &added); err != nil {
		return nil, err
	}
	out := make([]map[string]string, len(added))
	for i, t := range added {
		out[i] = map[string]string{"id": t.ID, "state": string(t.State)}
	}
	return map[string]any{"added": out}, nil
}

func plan(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	if _, err := s.me(ctx); err != nil {
		return nil, err
	}
	p, err := decodeArgs[struct{ Tasks []graph.Task }](raw)
	if err != nil {
		return nil, err
	}
	return addTasks(ctx, s, p.Tasks)
}

func propose(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	if _, err := s.me(ctx); err != nil {
		return nil, err
	}
	p, err := decodeArgs[struct {
		Parent string
		Tasks  []graph.Task
	}](raw)
	if err != nil {
		return nil, err
	}
	if err := s.call(ctx, "task.get", hub.IDParams{ID: p.Parent}, nil); err != nil {
		return nil, err
	}
	for i := range p.Tasks {
		p.Tasks[i].ParentID = p.Parent
	}
	return addTasks(ctx, s, p.Tasks)
}

func status(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	p, err := decodeArgs[struct{ Verbose bool }](raw)
	if err != nil {
		return nil, err
	}
	var snap hub.Snapshot
	if err := s.call(ctx, "status", nil, &snap); err != nil {
		return nil, err
	}
	if p.Verbose {
		return snap, nil
	}
	type agent struct {
		ID   string `json:"id"`
		Role string `json:"role"`
		Name string `json:"name,omitempty"`
	}
	type task struct {
		ID    string      `json:"id"`
		Title string      `json:"title,omitempty"`
		State graph.State `json:"state"`
		Owner string      `json:"owner,omitempty"`
		Deps  []string    `json:"deps,omitempty"`
	}
	agents := make([]agent, len(snap.Agents))
	for i, a := range snap.Agents {
		agents[i] = agent{a.ID, a.Role, a.Name}
	}
	tasks := make([]task, len(snap.Tasks))
	counts := map[graph.State]int{}
	for i, t := range snap.Tasks {
		tasks[i] = task{t.ID, t.Title, t.State, t.Owner, t.Deps}
		counts[t.State]++
	}
	leases := make([]string, len(snap.Leases))
	for i, l := range snap.Leases {
		leases[i] = fmt.Sprintf("%s %s %s", l.Holder, l.Mode, l.Path)
	}
	out := map[string]any{"agents": agents, "tasks": tasks, "counts": counts, "critical_path": snap.CriticalPath, "leases": leases}
	if b := snap.Swarm.Integration; b != "" {
		out["integration"] = b // worktree isolation: where finished tasks are merged
	}
	if u := snap.Usage; u != nil && u.Swarm.Total() > 0 {
		out["usage"] = fmt.Sprintf("%d tokens, ~$%.2f", u.Swarm.Total(), u.Swarm.CostUSD)
	}
	if snap.State != "" && snap.State != hub.RunRunning {
		out["state"] = snap.State // paused or stopped
	}
	return out, nil
}

func control(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	p, err := decodeArgs[struct{ Action, Target string }](raw)
	if err != nil {
		return nil, err
	}
	switch p.Action {
	case "cancel":
	case "spawn":
		var a hub.Agent
		if err := s.call(ctx, "agent.spawn", hub.SpawnParams{Adapter: p.Target}, &a); err != nil {
			return nil, err
		}
		return map[string]string{"spawned": a.ID, "adapter": a.Adapter}, nil
	case "retire":
		if p.Target == "" {
			return nil, errors.New("target (agent id) is required")
		}
		if err := s.call(ctx, "agent.retire", hub.RetireParams{Agent: p.Target}, nil); err != nil {
			return nil, err
		}
		return map[string]string{"retired": p.Target}, nil
	case hub.ActionPause, hub.ActionResume, hub.ActionStop:
		var r hub.ControlResult
		if err := s.call(ctx, "swarm.control", hub.ControlParams{Action: p.Action, By: me}, &r); err != nil {
			return nil, err
		}
		return map[string]string{"state": r.State}, nil
	default:
		return nil, fmt.Errorf("unknown action %q (cancel, spawn, retire, pause, resume or stop)", p.Action)
	}
	var t graph.Task
	if err := s.call(ctx, "task.state", hub.TaskStateParams{ID: p.Target, State: graph.Cancelled}, &t); err != nil {
		return nil, err
	}
	return map[string]string{"task": t.ID, "state": string(t.State)}, nil
}

func next(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	agent, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	p, err := decodeArgs[struct{ Wait float64 }](raw)
	if err != nil {
		return nil, err
	}
	// Paused or stopped: the hub hands out nothing, so don't wait for it.
	var snap hub.Snapshot
	if err := s.call(ctx, "status", nil, &snap); err != nil {
		return nil, err
	}
	switch snap.State {
	case hub.RunPaused:
		return map[string]any{"task": nil, "state": snap.State, "note": `swarm paused by the lead: tazzy_wait(until="msg") and try again`}, nil
	case hub.RunStopped:
		return map[string]any{"task": nil, "state": snap.State, "note": "swarm stopped: release your leases and exit now (return a one-line summary); do not call tazzy_next again"}, nil
	}
	// The hub scores and claims in one step (sched.next).
	var claimed *hub.TaskStateResult
	ok, err := s.waitFor(ctx, waitDuration(p.Wait, 0), func(ctx context.Context) (bool, error) {
		claimed = nil
		err := s.call(ctx, "sched.next", hub.AgentParams{Agent: agent}, &claimed)
		return claimed != nil, err
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]any{"task": nil, "hint": `no ready task for you; tazzy_wait(until="any") or tazzy_next(wait=45)`}, nil
	}
	s.setMine(claimed.ID, agent)

	// Hand over what the deps produced and what earlier attempts recorded.
	deps := map[string]*graph.Result{}
	for _, d := range claimed.Deps {
		var t graph.Task
		if err := s.call(ctx, "task.get", hub.IDParams{ID: d}, &t); err == nil && t.Result != nil {
			deps[d] = t.Result
		}
	}
	out := map[string]any{"task": handover(claimed.Task)}
	if claimed.Worktree != "" {
		out["worktree"] = claimed.Worktree
		out["worktree_note"] = "work only inside this directory: it is your own git worktree, on a branch for this task; paths and leases are relative to it. Don't commit, merge or switch branches: tazzy_done commits and merges your work"
	}
	if len(deps) > 0 {
		out["dep_results"] = deps
	}
	if claimed.Attempts > 1 {
		out["hint"] = "earlier attempts stopped: resume from the checkpoints instead of starting over, and check the files they touched"
	}
	return out, nil
}

// handoverTask is a claimed task as shown to its new owner: the spec plus,
// compactly, everything earlier attempts left behind (plan §3.7).
type handoverTask struct {
	ID           string   `json:"id"`
	Title        string   `json:"title,omitempty"`
	Spec         string   `json:"spec,omitempty"`
	Files        []string `json:"files,omitempty"`
	ParentID     string   `json:"parent_id,omitempty"`
	Attempts     int      `json:"attempts"`
	Checkpoints  []string `json:"checkpoints,omitempty"`   // "agent: note", oldest first
	FilesTouched []string `json:"files_touched,omitempty"` // by earlier attempts
	Failures     []string `json:"failures,omitempty"`      // "agent: why that attempt ended"
}

func handover(t graph.Task) handoverTask {
	h := handoverTask{ID: t.ID, Title: t.Title, Spec: t.Spec, Files: t.Files, ParentID: t.ParentID, Attempts: t.Attempts}
	line := func(n graph.Note) string {
		if n.Agent == "" {
			return n.Text
		}
		return n.Agent + ": " + n.Text
	}
	for _, n := range t.Checkpoints {
		h.Checkpoints = append(h.Checkpoints, line(n))
		for _, f := range n.Files {
			if !slices.Contains(h.FilesTouched, f) {
				h.FilesTouched = append(h.FilesTouched, f)
			}
		}
	}
	for _, n := range t.Failures {
		h.Failures = append(h.Failures, line(n))
	}
	return h
}

func progress(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	agent, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	p, err := decodeArgs[struct {
		Task, Note   string
		FilesTouched []string `json:"files_touched"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if err := s.call(ctx, "task.note", hub.TaskNoteParams{ID: p.Task, Agent: agent, Text: p.Note, Files: p.FilesTouched}, nil); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil
}

func done(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	agent, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	p, err := decodeArgs[struct {
		Task, Summary string
		Files         []string
		Artifacts     map[string]string
	}](raw)
	if err != nil {
		return nil, err
	}
	r := &graph.Result{Summary: p.Summary, Files: p.Files, Artifacts: p.Artifacts}
	if err := s.call(ctx, "task.state", hub.TaskStateParams{ID: p.Task, State: graph.Done, Agent: agent, Result: r}, nil); err != nil {
		return nil, err
	}
	s.setMine(p.Task, "")
	return map[string]string{"task": p.Task, "state": string(graph.Done), "next": "tazzy_next"}, nil
}

func fail(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	agent, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	p, err := decodeArgs[struct {
		Task, Reason string
		Retryable    bool
	}](raw)
	if err != nil {
		return nil, err
	}
	var r hub.FailResult
	if err := s.call(ctx, "task.fail", hub.TaskFailParams{ID: p.Task, Agent: agent, Reason: p.Reason, Retryable: p.Retryable}, &r); err != nil {
		return nil, err
	}
	s.setMine(p.Task, "")
	out := map[string]any{"task": p.Task, "state": r.Task.State, "next": "tazzy_next"}
	if r.Task.State == graph.Failed {
		out["note"] = "the lead has been told"
	}
	return out, nil
}

func send(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	agent, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	p, err := decodeArgs[struct {
		To, Kind, Body string
		ReplyTo        string `json:"reply_to"`
	}](raw)
	if err != nil {
		return nil, err
	}
	// A client-chosen id makes the reconnect retry in session.call idempotent.
	id := make([]byte, 6)
	rand.Read(id)
	var m bus.Message
	err = s.call(ctx, "msg.send", hub.SendParams{ID: agent + "-" + hex.EncodeToString(id), From: agent, To: p.To, Kind: p.Kind, Body: p.Body, ReplyTo: p.ReplyTo}, &m)
	if err != nil {
		return nil, err
	}
	return map[string]string{"id": m.ID}, nil
}

func wait(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	agent, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	p, err := decodeArgs[struct {
		Until   string
		Timeout float64
	}](raw)
	if err != nil {
		return nil, err
	}
	d := waitDuration(p.Timeout, defaultWait)
	out := map[string]any{"until": p.Until}
	var check func(context.Context) (bool, error)
	id, isTask := strings.CutPrefix(p.Until, "task:")
	path, isLease := strings.CutPrefix(p.Until, "lease:")
	switch {
	case isLease:
		// Met when a write lease on path would be granted to the caller.
		check = func(ctx context.Context) (bool, error) {
			var res struct{ Conflicts []lease.Conflict }
			if err := s.call(ctx, "lease.check", hub.LeaseCheckParams{Agent: agent, Paths: []string{path}}, &res); err != nil {
				return false, err
			}
			out["held_by"] = holders(res.Conflicts)
			return len(res.Conflicts) == 0, nil
		}
	case isTask:
		check = func(ctx context.Context) (bool, error) {
			var t graph.Task
			if err := s.call(ctx, "task.get", hub.IDParams{ID: id}, &t); err != nil {
				return false, err
			}
			out["state"] = t.State
			return t.State == graph.Done || t.State == graph.Failed || t.State == graph.Cancelled, nil
		}
	case p.Until == "msg":
		check = func(ctx context.Context) (bool, error) { return s.pending(ctx, agent) }
	case p.Until == "any":
		s.mu.Lock()
		seen := s.taskEvents
		s.mu.Unlock()
		check = func(ctx context.Context) (bool, error) {
			s.mu.Lock()
			changed := s.taskEvents != seen
			s.mu.Unlock()
			if changed {
				return true, nil
			}
			return s.pending(ctx, agent)
		}
	default:
		return nil, fmt.Errorf(`until must be task:<id>, msg, lease:<path> or any, got %q`, p.Until)
	}
	met, err := s.waitFor(ctx, d, check)
	if err != nil {
		return nil, err
	}
	out["met"] = met
	return out, nil
}

// holders lists who holds the conflicting leases, once each.
func holders(c []lease.Conflict) []string {
	out := []string{}
	for _, x := range c {
		if !slices.Contains(out, x.Lease.Holder) {
			out = append(out, x.Lease.Holder)
		}
	}
	return out
}

func leaseOp(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	agent, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	p, err := decodeArgs[struct {
		Paths   []string
		Mode    lease.Mode
		Release bool
	}](raw)
	if err != nil {
		return nil, err
	}
	if p.Release {
		var res struct{ Released []lease.Lease }
		if err := s.call(ctx, "lease.release", hub.LeaseReleaseParams{Agent: agent, Paths: p.Paths}, &res); err != nil {
			return nil, err
		}
		return map[string]any{"released": leasePaths(res.Released)}, nil
	}
	if len(p.Paths) == 0 {
		return nil, errors.New("paths is required")
	}
	var res struct{ Leases []lease.Lease }
	err = s.call(ctx, "lease.acquire", hub.LeaseParams{Agent: agent, Paths: p.Paths, Mode: p.Mode}, &res)
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code == hub.CodeConflict {
		// Not a tool error: a conflict is an expected outcome to act on.
		var c []lease.Conflict
		json.Unmarshal(pe.Data, &c)
		return map[string]any{
			"granted":   false,
			"conflicts": pe.Message,
			"held_by":   holders(c),
			"hint":      `nothing was leased; tazzy_wait(until="lease:<path>"), work on something else, or tazzy_send the holder`,
		}, nil
	}
	if err != nil {
		return nil, err
	}
	held := make([]string, len(res.Leases))
	for i, l := range res.Leases {
		held[i] = string(l.Mode) + " " + l.Path
	}
	return map[string]any{"granted": true, "held": held}, nil
}

func leasePaths(ls []lease.Lease) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.Path
	}
	return out
}

func boardOp(ctx context.Context, s *session, raw json.RawMessage) (any, error) {
	agent, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	p, err := decodeArgs[struct{ Op, Key, Value string }](raw)
	if err != nil {
		return nil, err
	}
	var res any
	switch p.Op {
	case "get":
		err = s.call(ctx, "board.get", hub.KeyParams{Key: p.Key}, &res)
	case "set":
		err = s.call(ctx, "board.set", hub.BoardSetParams{Key: p.Key, Value: p.Value, By: agent}, &res)
	case "list":
		err = s.call(ctx, "board.list", hub.PrefixParams{Prefix: p.Key}, &res)
	case "decide":
		err = s.call(ctx, "board.decide", hub.DecideParams{Text: p.Value, By: agent}, &res)
	default:
		return nil, fmt.Errorf("unknown op %q", p.Op)
	}
	if err != nil {
		return nil, err
	}
	if res == nil {
		return map[string]any{"key": p.Key, "value": nil}, nil
	}
	return res, nil
}
