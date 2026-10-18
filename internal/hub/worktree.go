package hub

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"tazzyronth/internal/config"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/worktree"
)

// Worktree isolation (plan §3.4). Every git operation runs with h.mu held:
// it keeps merges, claims and the integration branch in one order (a task's
// dependents are claimed only after it has been merged) at the cost of
// blocking the hub for the few git calls each claim and done takes.

func (h *Hub) isolation() string { return cmp.Or(h.st.swarm.Isolation, h.cfg.Isolation) }

// treesLocked returns the repo in worktree isolation, or nil.
func (h *Hub) treesLocked() *worktree.Repo {
	if h.repo == nil && h.isolation() == config.IsolationWorktree {
		h.repo, _ = worktree.Open(h.st.root)
	}
	return h.repo
}

// integrationLocked returns the repo and integration branch in worktree
// isolation (nil, "" otherwise), creating the branch from HEAD on first use
// so the user's checked-out branch is never touched.
func (h *Hub) integrationLocked() (*worktree.Repo, string, error) {
	if h.isolation() != config.IsolationWorktree {
		return nil, "", nil
	}
	if h.repo == nil {
		r, err := worktree.Open(h.st.root)
		if err != nil {
			return nil, "", err
		}
		h.repo = r
	}
	if b := h.st.swarm.Integration; b != "" {
		return h.repo, b, nil
	}
	name := "tazzy/integration-" + time.Now().UTC().Format("20060102-150405")
	if err := h.repo.NewIntegration(name); err != nil {
		return nil, "", err
	}
	p := h.st.swarm
	p.Integration = name
	if _, err := h.mutateLocked(evSwarm, p); err != nil {
		return nil, "", err
	}
	return h.repo, name, nil
}

// workdir is where agent's files are: its worktree if it has one, else "".
func (h *Hub) workdir(agent string) string {
	if h.repo == nil {
		return ""
	}
	if d := h.repo.Dir(agent); worktree.Exists(d) {
		return d
	}
	return ""
}

// startLocked puts a just-claimed task on a fresh branch in its owner's
// worktree. If that fails the claim is undone as a retryable failure.
func (h *Hub) startLocked(r *TaskStateResult) error {
	repo, integ, err := h.integrationLocked()
	if err != nil || repo == nil {
		return err
	}
	if r.Worktree, err = repo.Start(r.Owner, r.ID, integ, r.Merge); err != nil {
		h.mutateLocked(evTaskFail, TaskFailParams{ID: r.ID, Agent: r.Owner, Reason: "worktree: " + err.Error(), Retryable: true, MaxAttempts: h.cfg.Retries.MaxAttempts})
		return fmt.Errorf("prepare worktree: %w", err)
	}
	return nil
}

// mergeLocked runs before task p.ID is marked done: the owner's work is
// committed and merged into the integration branch. A conflict becomes a
// new task that blocks the task's dependents until it is resolved.
func (h *Hub) mergeLocked(p TaskStateParams) error {
	t, ok := h.st.graph.Get(p.ID)
	if !ok || !active(t.State) || checkOwner(t, p.Agent) != nil {
		return nil // the state change itself will fail
	}
	integ := h.st.swarm.Integration
	if h.treesLocked() == nil || integ == "" || h.workdir(t.Owner) == "" || !h.repo.On(t.Owner, t.ID) {
		return nil
	}
	msg := fmt.Sprintf("tazzy: %s (%s)", cmp.Or(t.Title, t.ID), t.Owner)
	if p.Result != nil && p.Result.Summary != "" {
		msg += "\n\n" + p.Result.Summary
	}
	conflicts, err := h.repo.Finish(t.Owner, t.ID, integ, t.Merge, msg)
	if err != nil || conflicts == nil {
		return err
	}

	id := "merge-" + t.ID
	for n := 2; ; n++ {
		if _, exists := h.st.graph.Get(id); !exists {
			break
		}
		id = fmt.Sprintf("merge-%s-%d", t.ID, n)
	}
	var blocks []string
	for _, d := range h.st.graph.List() {
		if d.State == graph.Pending && slices.Contains(d.Deps, t.ID) {
			blocks = append(blocks, d.ID)
		}
	}
	branch := worktree.Branch(t.ID)
	spec := fmt.Sprintf("Merging %s (task %s) into the integration branch %s conflicted in: %s. "+
		"Your worktree has %s merged into %s with the conflicts left in place (the merge is in progress). "+
		"Resolve every conflict so that both sides' changes survive (read task %s's spec and result), check that it still builds, then tazzy_done: "+
		"the hub commits the merge and merges it into %s. Don't commit, abort the merge or switch branches yourself.",
		branch, t.ID, integ, strings.Join(conflicts, ", "), branch, integ, t.ID, integ)
	res := graph.Task{ID: id, Title: "resolve conflict merging " + t.ID, Spec: spec, Files: conflicts,
		Caps: t.Caps, Priority: t.Priority + 1, ParentID: t.ID, Merge: branch}
	if _, err := h.mutateLocked(evTaskAdd, TaskAddParams{Tasks: []graph.Task{res}, Blocks: blocks}); err != nil {
		return fmt.Errorf("add conflict task: %w", err)
	}
	h.notifyLeadLocked(fmt.Sprintf("merging task %s conflicted in %s; task %s will resolve it", t.ID, strings.Join(conflicts, ", "), id))
	return nil
}

// abandonLocked keeps an unfinished attempt's work on a renamed branch.
func (h *Hub) abandonLocked(agent string, t graph.Task) {
	if h.treesLocked() == nil {
		return
	}
	if err := h.repo.Abandon(agent, t.ID, t.Attempts); err != nil {
		fmt.Fprintf(os.Stderr, "tazzy hub: keep %s's work on %s: %v\n", agent, t.ID, err)
	}
}

// removeTreeLocked deletes agent's worktree, unless its process is still
// running there (it is removed once the process has exited).
func (h *Hub) removeTreeLocked(agent string) {
	if h.treesLocked() == nil || h.sup.Has(agent) {
		return
	}
	if err := h.repo.Remove(agent); err != nil {
		fmt.Fprintf(os.Stderr, "tazzy hub: remove worktree of %s: %v\n", agent, err)
	}
}

// cleanTreesLocked prunes git's worktree records and removes the worktrees
// of agents that are gone (hub start).
func (h *Hub) cleanTreesLocked() {
	if h.treesLocked() == nil {
		return
	}
	h.repo.Prune()
	for _, a := range h.repo.Agents() {
		if h.st.agents[a] == nil {
			h.removeTreeLocked(a)
		}
	}
}
