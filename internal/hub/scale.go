package hub

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"tazzyronth/internal/config"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/supervisor"
	"tazzyronth/internal/worktree"
)

// Params of the supervisor RPCs.
type (
	SpawnParams struct {
		Adapter string `json:"adapter,omitempty"` // default: the swarm's, else the config's
	}
	RetireParams struct {
		Agent  string `json:"agent"`
		Reason string `json:"reason,omitempty"`
	}
)

// Reasons an idle worker is released rather than removed for misbehaving;
// a released worker is told to stop for good instead of rejoining.
const (
	ReasonFinished = "swarm finished"
	ReasonPrevious = "left over from a previous swarm"
)

var released = map[string]bool{ReasonFinished: true, ReasonPrevious: true}

func (h *Hub) scaleLoop() {
	defer h.wg.Done()
	t := time.NewTicker(config.Seconds(h.cfg.Timeouts.ScaleTick))
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case now := <-t.C:
			h.scale(now)
		}
	}
}

// spawnMode is the effective mode: config.SpawnHub or config.SpawnHost.
func (h *Hub) spawnMode() string {
	return config.ResolveSpawn(cmp.Or(h.st.swarm.Spawn, h.cfg.SpawnMode), h.st.swarm.Client)
}

func (h *Hub) maxAgents() int { return cmp.Or(h.st.swarm.MaxAgents, h.cfg.MaxAgents) }

// scale is one tick of elastic scaling (plan §3.3): in hub spawn mode, spawn
// a worker (at most one per cooldown) while fewer are alive than
//
//	desired = clamp(min_agents, busy workers + parallel width of ready tasks, max_agents)
//
// and, in any mode, retire hub-spawned workers that have been idle for
// idle_retire_s while more are alive than desired. Once every task has ended
// and stayed so for finish_grace_s (room for a late tazzy_propose), all
// workers are released, host subagents included, so the next swarm in this
// project starts from none. It also notices when the hub has been idle long
// enough to exit.
func (h *Hub) scale(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	live := h.sup.Live()
	owners := map[string]bool{}
	for _, t := range h.st.graph.List() {
		if active(t.State) {
			owners[t.Owner] = true
		}
	}
	busy := 0
	for _, id := range live {
		switch {
		case h.st.agents[id] == nil: // left (agent.leave) behind the supervisor's back
			h.sup.Kill(id)
		case owners[id]:
			busy++
			delete(h.idleSince, id)
		case h.idleSince[id].IsZero():
			h.idleSince[id] = now
		}
	}

	desired := 0
	if h.spawnMode() == config.SpawnHub && h.st.running() { // paused or stopped: spawn nothing
		desired = min(max(busy+h.st.width(), h.cfg.MinAgents), h.maxAgents())
	}
	n := len(live)
	if n < desired && now.Sub(h.lastSpawn) >= config.Seconds(h.cfg.Timeouts.SpawnCooldown) &&
		h.st.spawned < h.cfg.Budget.MaxTotalSpawns {
		if _, err := h.spawnLocked(""); err != nil {
			fmt.Fprintf(os.Stderr, "tazzy hub: spawn: %v\n", err)
		}
		h.lastSpawn = now // also after a failure, so a broken adapter isn't retried every tick
	}
	for _, id := range live { // sorted, so retirement order is deterministic
		if n <= desired {
			break
		}
		if since, ok := h.idleSince[id]; ok && h.st.agents[id] != nil && now.Sub(since) >= config.Seconds(h.cfg.Timeouts.IdleRetire) {
			if err := h.retireLocked(id, "idle"); err != nil {
				fmt.Fprintf(os.Stderr, "tazzy hub: retire %s: %v\n", id, err)
			}
			n--
		}
	}

	if !h.st.finished() {
		h.doneFrom = time.Time{}
	} else if h.doneFrom.IsZero() {
		h.doneFrom = now
	} else if now.Sub(h.doneFrom) >= config.Seconds(h.cfg.Timeouts.FinishGrace) {
		h.pruneLocked(ReasonFinished)
	}

	// Hub-spawned workers don't count: with every host gone nobody is using
	// the swarm, so it shuts down (Close kills the workers) instead of
	// running on unattended.
	if h.hostConnsLocked() > 0 || len(h.taps) > 0 {
		h.quietFrom = now
	} else if now.Sub(h.quietFrom) >= config.Seconds(h.cfg.Timeouts.HubIdle) {
		select {
		case <-h.idle:
		default:
			close(h.idle)
		}
	}
}

// hostConnsLocked counts the connections that aren't hub-spawned workers'.
// Called with h.mu held.
func (h *Hub) hostConnsLocked() int {
	n := 0
	for c := range h.conns {
		if !c.worker {
			n++
		}
	}
	return n
}

// spawnLocked pre-registers a worker and starts its process. Called with h.mu held.
func (h *Hub) spawnLocked(adapter string) (Agent, error) {
	name := h.cfg.WorkerAdapter(cmp.Or(adapter, h.st.swarm.Adapter), h.st.swarm.Client)
	a, ok := h.cfg.Adapters[name]
	if !ok {
		return Agent{}, fmt.Errorf("unknown adapter %q", name)
	}
	if h.st.run == RunStopped {
		return Agent{}, errors.New("swarm is stopped")
	}
	if h.st.spawned >= h.cfg.Budget.MaxTotalSpawns {
		return Agent{}, fmt.Errorf("spawn budget exhausted (%d workers)", h.cfg.Budget.MaxTotalSpawns)
	}
	if n := len(h.sup.Live()); n >= h.maxAgents() {
		return Agent{}, fmt.Errorf("already %d workers (max_agents %d)", n, h.maxAgents())
	}
	// The id goes into the event so the log shows which process is which.
	res, err := h.mutateLocked(evJoin, JoinParams{ID: h.st.nextID(), Name: name, Role: "worker", Adapter: name})
	if err != nil {
		return Agent{}, err
	}
	ag := res.(Agent)
	goal := ""
	if e, ok := h.st.board.Get("goal"); ok {
		goal = e.Value
	}
	workdir := ""
	repo, integ, err := h.integrationLocked()
	if err == nil && repo != nil {
		workdir, err = repo.Ensure(ag.ID, integ)
	}
	if err == nil {
		err = h.sup.Spawn(ag.ID, a, supervisor.Prompt(ag.ID, cmp.Or(workdir, h.st.root), goal, a.Note), workdir)
	}
	if err != nil {
		h.mutateLocked(evLeave, LeaveParams{Agent: ag.ID, Reason: "spawn failed: " + err.Error()})
		return Agent{}, err
	}
	h.idleSince[ag.ID] = time.Now()
	return ag, nil
}

// retireLocked removes an agent (requeueing any task it holds) and kills its
// process if the hub spawned it. Called with h.mu held.
func (h *Hub) retireLocked(agent, reason string) error {
	if _, err := h.leaveLocked(agent, cmp.Or(reason, "retired")); err != nil {
		return err
	}
	delete(h.idleSince, agent)
	h.sup.Kill(agent)
	return nil
}

// pruneLocked releases every non-lead agent that holds no task and no lease,
// returning their ids. Called with h.mu held.
func (h *Hub) pruneLocked(reason string) []string {
	busy := map[string]bool{}
	for _, t := range h.st.graph.List() {
		if active(t.State) || t.State == graph.Review {
			busy[t.Owner] = true
		}
	}
	for _, l := range h.st.leases.List() {
		busy[l.Holder] = true
	}
	out := []string{}
	for _, a := range h.st.agentList() {
		if a.Role == "lead" || busy[a.ID] {
			continue
		}
		if err := h.retireLocked(a.ID, reason); err != nil {
			fmt.Fprintf(os.Stderr, "tazzy hub: release %s: %v\n", a.ID, err)
			continue
		}
		out = append(out, a.ID)
	}
	return out
}

// workerExited is the supervisor's exit callback. A worker that exits while
// still registered has died: it leaves, which releases its leases and
// requeues its task (or fails it, out of attempts).
func (h *Hub) workerExited(agent string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.idleSince, agent)
	if h.closed {
		return
	}
	if h.st.agents[agent] == nil { // already left (retired, stalled): its worktree can go now
		h.removeTreeLocked(agent)
		return
	}
	reason := "exited"
	if err != nil {
		reason += ": " + err.Error()
	}
	if _, err := h.leaveLocked(agent, reason); err != nil {
		fmt.Fprintf(os.Stderr, "tazzy hub: %s exited: %v\n", agent, err)
	}
}

func agentSpawn(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
	p, err := decodeParams[SpawnParams](raw)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errors.New("hub is closing")
	}
	a, err := h.spawnLocked(p.Adapter)
	if err != nil {
		return nil, err
	}
	h.lastSpawn = time.Now()
	return a, nil
}

func agentRetire(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
	p, err := decodeParams[RetireParams](raw)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.retireLocked(p.Agent, p.Reason); err != nil {
		return nil, err
	}
	return map[string]string{"retired": p.Agent}, nil
}

// agentPrune releases the idle workers left over from an earlier swarm;
// tazzy_start calls it so they don't count against the new one's max_agents.
func agentPrune(h *Hub, _ *conn, _ json.RawMessage) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errors.New("hub is closing")
	}
	return map[string][]string{"released": h.pruneLocked(ReasonPrevious)}, nil
}

// swarmSet records the lead's settings and returns the effective ones
// (config defaults filled in, spawn mode resolved).
func swarmSet(h *Hub, _ *conn, raw json.RawMessage) (any, error) {
	p, err := decodeParams[SwarmParams](raw)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if cmp.Or(p.Isolation, h.cfg.Isolation) == config.IsolationWorktree && h.repo == nil {
		r, err := worktree.Open(h.st.root) // fail before recording a mode that can't work
		if err != nil {
			return nil, err
		}
		h.repo = r
	}
	if _, err := h.mutateLocked(evSwarm, p); err != nil {
		return nil, err
	}
	_, integ, err := h.integrationLocked()
	if err != nil {
		return nil, err
	}
	return SwarmParams{
		Isolation:   h.isolation(),
		Integration: integ,
		Spawn:       h.spawnMode(),
		MaxAgents:   h.maxAgents(),
		Adapter:     h.cfg.WorkerAdapter(h.st.swarm.Adapter, h.st.swarm.Client),
		Client:      h.st.swarm.Client,
	}, nil
}
