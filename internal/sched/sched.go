// Package sched scores ready tasks for an agent asking for work (plan §3.2).
// It is pure: the hub gathers the inputs and claims the winner.
package sched

import (
	"slices"

	"tazzyronth/internal/graph"
	"tazzyronth/internal/lease"
)

type Weights struct {
	Priority     float64 `json:"priority"`      // per point of task priority
	CriticalPath float64 `json:"critical_path"` // per task on the longest chain the task unblocks
	CapMatch     float64 `json:"cap_match"`     // per required capability: specialists take specialised work first
	Affinity     float64 `json:"affinity"`      // per declared file the agent holds or has worked on
	Conflict     float64 `json:"conflict"`      // per declared file leased by someone else
	Attempts     float64 `json:"attempts"`      // per earlier attempt
	Refail       float64 `json:"refail"`        // the agent already failed or gave up this task
}

// Defaults are the plan §8 weights; config scheduler.weights overrides them.
var Defaults = Weights{Priority: 3, CriticalPath: 2, CapMatch: 5, Affinity: 2, Conflict: 4, Attempts: 1, Refail: 10}

type Candidate struct {
	Task      graph.Task
	Height    int // from graph.Heights
	Conflicts int // declared files that collide with other agents' leases
	FailedBy  []string
}

type Agent struct {
	ID    string
	Caps  []string
	Files []string // normalized paths it holds leases on or has changed
}

// Score rates c for a. ok is false if a lacks a required capability.
func Score(w Weights, c Candidate, a Agent) (score float64, ok bool) {
	t := c.Task
	for _, cp := range t.Caps {
		if !slices.Contains(a.Caps, cp) {
			return 0, false
		}
	}
	affinity := 0
	for _, f := range t.Files {
		if slices.ContainsFunc(a.Files, func(g string) bool { return lease.Overlap(f, g) }) {
			affinity++
		}
	}
	score = w.Priority*float64(t.Priority) +
		w.CriticalPath*float64(c.Height) +
		w.CapMatch*float64(len(t.Caps)) +
		w.Affinity*float64(affinity) -
		w.Conflict*float64(c.Conflicts) -
		w.Attempts*float64(t.Attempts)
	if slices.Contains(c.FailedBy, a.ID) {
		score -= w.Refail
	}
	return score, true
}

// Pick returns the index of the best candidate for a, or -1 if a can do none
// of them. A task without lease conflicts always beats one with conflicts,
// whatever the weights: the agent couldn't start the latter anyway. Otherwise
// the higher score wins, and ties go to the smallest task id, so the choice is
// deterministic.
func Pick(w Weights, cands []Candidate, a Agent) int {
	best, bestScore := -1, 0.0
	for i, c := range cands {
		s, ok := Score(w, c, a)
		if !ok {
			continue
		}
		if best < 0 {
			best, bestScore = i, s
			continue
		}
		free, bestFree := c.Conflicts == 0, cands[best].Conflicts == 0
		if free != bestFree {
			if free {
				best, bestScore = i, s
			}
			continue
		}
		if s > bestScore || s == bestScore && c.Task.ID < cands[best].Task.ID {
			best, bestScore = i, s
		}
	}
	return best
}
