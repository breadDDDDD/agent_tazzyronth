// Package graph is the swarm's task DAG: tasks, dependency edges, state
// transitions, automatic readiness and critical-path ranking.
package graph

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

type State string

const (
	Pending   State = "pending" // waiting on deps
	Ready     State = "ready"   // all deps done, unclaimed
	Claimed   State = "claimed"
	Running   State = "running"
	Blocked   State = "blocked" // owner is stuck (question, lease, ...)
	Review    State = "review"
	Done      State = "done"
	Failed    State = "failed"
	Cancelled State = "cancelled"
)

// transitions lists the states each state may move to via SetState.
// pending→ready is automatic only; done and cancelled are terminal.
var transitions = map[State][]State{
	Pending: {Cancelled},
	Ready:   {Claimed, Cancelled},
	Claimed: {Running, Ready, Blocked, Review, Done, Failed, Cancelled},
	Running: {Blocked, Review, Ready, Done, Failed, Cancelled},
	Blocked: {Running, Ready, Failed, Cancelled},
	Review:  {Running, Done, Failed, Cancelled},
	Failed:  {Ready, Cancelled}, // retry
}

type Task struct {
	ID          string   `json:"id"`
	Title       string   `json:"title,omitempty"`
	Spec        string   `json:"spec,omitempty"`
	Deps        []string `json:"deps,omitempty"`
	Caps        []string `json:"caps,omitempty"`
	Files       []string `json:"files,omitempty"`
	Priority    int      `json:"priority,omitempty"`
	State       State    `json:"state"`
	Owner       string   `json:"owner,omitempty"`
	Attempts    int      `json:"attempts,omitempty"`
	MaxAttempts int      `json:"max_attempts,omitempty"`
	Checkpoints []Note   `json:"checkpoints,omitempty"`
	Failures    []Note   `json:"failures,omitempty"` // why earlier attempts ended, oldest first
	Result      *Result  `json:"result,omitempty"`
	ParentID    string   `json:"parent_id,omitempty"`
	// Merge is a branch merged (uncommitted, conflicts and all) into the
	// task's worktree when it is claimed: set on worktree conflict-resolution
	// tasks.
	Merge string `json:"merge,omitempty"`
}

// Note is a progress checkpoint, handed to the next owner on reassignment.
type Note struct {
	Time  time.Time `json:"time"`
	Agent string    `json:"agent,omitempty"`
	Text  string    `json:"text"`
	Files []string  `json:"files,omitempty"` // files touched so far
}

type Result struct {
	Summary   string            `json:"summary,omitempty"`
	Files     []string          `json:"files,omitempty"`
	Artifacts map[string]string `json:"artifacts,omitempty"`
}

var ErrNotFound = errors.New("task not found")

// Graph is not safe for concurrent use.
type Graph struct {
	tasks    map[string]*Task
	children map[string][]string // dep id -> ids of tasks that depend on it
}

func New() *Graph {
	return &Graph{tasks: map[string]*Task{}, children: map[string][]string{}}
}

// Add inserts a batch of new tasks atomically. Deps may point at existing
// tasks or at others in the batch. State, owner and progress fields of the
// inputs are reset; tasks whose deps are all done start ready.
func (g *Graph) Add(tasks ...Task) error {
	batch := make(map[string]*Task, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		switch {
		case t.ID == "":
			return errors.New("task id is required")
		case g.tasks[t.ID] != nil || batch[t.ID] != nil:
			return fmt.Errorf("task %q already exists", t.ID)
		}
		batch[t.ID] = t
	}
	lookup := func(id string) *Task {
		if t := batch[id]; t != nil {
			return t
		}
		return g.tasks[id]
	}
	for _, t := range tasks {
		for _, d := range t.Deps {
			if lookup(d) == nil {
				return fmt.Errorf("task %q: unknown dep %q", t.ID, d)
			}
		}
	}
	// Existing tasks gain edges only through AddDeps, which checks for cycles
	// itself, so any new cycle runs through the batch.
	if c := findCycle(tasks, func(id string) []string { return lookup(id).Deps }); c != nil {
		return fmt.Errorf("dependency cycle: %s", strings.Join(c, " -> "))
	}

	for _, in := range tasks {
		t := clone(in)
		t.State, t.Owner, t.Attempts, t.Checkpoints, t.Failures, t.Result = Pending, "", 0, nil, nil, nil
		g.tasks[t.ID] = t
		for _, d := range t.Deps {
			g.children[d] = append(g.children[d], t.ID)
		}
	}
	for _, t := range tasks {
		g.promote(t.ID)
	}
	return nil
}

// findCycle returns a cycle reachable from roots as a path that starts and
// ends at the same id, or nil.
func findCycle(roots []Task, deps func(string) []string) []string {
	const (
		visiting = 1
		visited  = 2
	)
	color := map[string]int{}
	var stack []string
	var visit func(id string) []string
	visit = func(id string) []string {
		switch color[id] {
		case visiting:
			i := slices.Index(stack, id)
			return append(slices.Clone(stack[i:]), id)
		case visited:
			return nil
		}
		color[id] = visiting
		stack = append(stack, id)
		for _, d := range deps(id) {
			if c := visit(d); c != nil {
				return c
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = visited
		return nil
	}
	for _, t := range roots {
		if c := visit(t.ID); c != nil {
			return c
		}
	}
	return nil
}

// promote moves a pending task to ready if all its deps are done.
func (g *Graph) promote(id string) {
	t := g.tasks[id]
	if t.State != Pending {
		return
	}
	for _, d := range t.Deps {
		if g.tasks[d].State != Done {
			return
		}
	}
	t.State = Ready
}

// SetState moves a task to state to. Claiming requires an owner and counts an
// attempt; returning to ready releases the owner. Completing a task promotes
// any dependents whose deps are now all done.
func (g *Graph) SetState(id string, to State, owner string) error {
	t := g.tasks[id]
	if t == nil {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if !slices.Contains(transitions[t.State], to) {
		return fmt.Errorf("task %q: cannot move from %s to %s", id, t.State, to)
	}
	switch to {
	case Claimed:
		if owner == "" {
			return fmt.Errorf("task %q: claim needs an owner", id)
		}
		t.Owner = owner
		t.Attempts++
	case Ready:
		t.Owner = ""
	}
	t.State = to
	if to == Done {
		for _, c := range g.children[id] {
			g.promote(c)
		}
	}
	return nil
}

func (g *Graph) AddNote(id string, n Note) error {
	t := g.tasks[id]
	if t == nil {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	t.Checkpoints = append(t.Checkpoints, n)
	return nil
}

func (g *Graph) AddFailure(id string, n Note) error {
	t := g.tasks[id]
	if t == nil {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	t.Failures = append(t.Failures, n)
	return nil
}

// FailDependents marks every pending task that depends, directly or
// transitively, on id (which has failed for good) as failed too, so the graph
// still reaches an end. It returns their ids in order.
func (g *Graph) FailDependents(id string) []string {
	var out []string
	var walk func(id string)
	walk = func(id string) {
		for _, c := range g.children[id] {
			if t := g.tasks[c]; t.State == Pending {
				t.State = Failed
				out = append(out, c)
				walk(c)
			}
		}
	}
	walk(id)
	slices.Sort(out)
	return out
}

func (g *Graph) SetResult(id string, r *Result) error {
	t := g.tasks[id]
	if t == nil {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	t.Result = clone(Task{Result: r}).Result
	return nil
}

// Get returns a copy of the task.
func (g *Graph) Get(id string) (Task, bool) {
	t := g.tasks[id]
	if t == nil {
		return Task{}, false
	}
	return *clone(*t), true
}

// Owner returns the task's current owner, or "" if it has none or doesn't exist.
func (g *Graph) Owner(id string) string {
	if t := g.tasks[id]; t != nil {
		return t.Owner
	}
	return ""
}

// List returns copies of all tasks sorted by id.
func (g *Graph) List() []Task {
	out := make([]Task, 0, len(g.tasks))
	for _, id := range g.ids() {
		out = append(out, *clone(*g.tasks[id]))
	}
	return out
}

func (g *Graph) ids() []string {
	ids := make([]string, 0, len(g.tasks))
	for id := range g.tasks {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func remaining(t *Task) bool { return t.State != Done && t.State != Cancelled }

// Heights returns, for every remaining (not done or cancelled) task, the
// number of tasks on the longest chain of remaining work that starts at it and
// runs through its dependents. A scheduler favours tasks with larger heights.
func (g *Graph) Heights() map[string]int {
	h := map[string]int{}
	var height func(id string) int
	height = func(id string) int {
		if v, ok := h[id]; ok {
			return v
		}
		best := 0
		for _, c := range g.children[id] {
			if remaining(g.tasks[c]) {
				best = max(best, height(c))
			}
		}
		h[id] = best + 1
		return h[id]
	}
	for id, t := range g.tasks {
		if remaining(t) {
			height(id)
		}
	}
	return h
}

// CriticalPath returns the longest chain of remaining tasks, first task
// first. Ties break by smallest id so the result is deterministic.
func (g *Graph) CriticalPath() []string {
	h := g.Heights()
	pick := func(cands []string) string {
		best := ""
		for _, id := range cands {
			if v, ok := h[id]; ok && (best == "" || v > h[best] || v == h[best] && id < best) {
				best = id
			}
		}
		return best
	}
	var path []string
	for id := pick(g.ids()); id != ""; id = pick(g.children[id]) {
		path = append(path, id)
	}
	return path
}

func clone(t Task) *Task {
	t.Deps = slices.Clone(t.Deps)
	t.Caps = slices.Clone(t.Caps)
	t.Files = slices.Clone(t.Files)
	t.Checkpoints = slices.Clone(t.Checkpoints)
	t.Failures = slices.Clone(t.Failures)
	if t.Result != nil {
		r := *t.Result
		r.Files = slices.Clone(r.Files)
		r.Artifacts = maps.Clone(r.Artifacts)
		t.Result = &r
	}
	return &t
}

// AddDeps makes pending task id also wait for deps (existing tasks), e.g. a
// task created to unblock it.
func (g *Graph) AddDeps(id string, deps ...string) error {
	t := g.tasks[id]
	if t == nil {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if t.State != Pending {
		return fmt.Errorf("task %q is %s, not pending", id, t.State)
	}
	for _, d := range deps {
		if g.tasks[d] == nil {
			return fmt.Errorf("task %q: unknown dep %q", id, d)
		}
	}
	all := append(slices.Clone(t.Deps), deps...)
	if c := findCycle([]Task{{ID: id}}, func(x string) []string {
		if x == id {
			return all
		}
		return g.tasks[x].Deps
	}); c != nil {
		return fmt.Errorf("dependency cycle: %s", strings.Join(c, " -> "))
	}
	for _, d := range deps {
		if !slices.Contains(t.Deps, d) {
			t.Deps = append(t.Deps, d)
			g.children[d] = append(g.children[d], id)
		}
	}
	return nil
}
