package sched

import (
	"testing"

	"tazzyronth/internal/graph"
)

func TestPick(t *testing.T) {
	w := Defaults
	me := Agent{ID: "w1", Caps: []string{"go"}, Files: []string{"src/api/**"}}
	cand := func(id string, f func(*Candidate)) Candidate {
		c := Candidate{Task: graph.Task{ID: id}, Height: 1}
		if f != nil {
			f(&c)
		}
		return c
	}
	tests := []struct {
		name  string
		cands []Candidate
		want  string // "" = none
	}{
		{"empty", nil, ""},
		{"tie by id", []Candidate{cand("b", nil), cand("a", nil)}, "a"},
		{"priority", []Candidate{cand("a", nil), cand("b", func(c *Candidate) { c.Task.Priority = 1 })}, "b"},
		{"critical path", []Candidate{cand("a", nil), cand("b", func(c *Candidate) { c.Height = 3 })}, "b"},
		{"missing cap is a hard filter", []Candidate{cand("a", func(c *Candidate) { c.Task.Caps = []string{"rust"}; c.Task.Priority = 9 })}, ""},
		{"cap match beats generic", []Candidate{cand("a", nil), cand("b", func(c *Candidate) { c.Task.Caps = []string{"go"} })}, "b"},
		{"file affinity", []Candidate{cand("a", func(c *Candidate) { c.Task.Files = []string{"web/x.ts"} }), cand("b", func(c *Candidate) { c.Task.Files = []string{"src/api/h.go"} })}, "b"},
		{"lease conflict", []Candidate{cand("a", func(c *Candidate) { c.Conflicts = 1 }), cand("b", nil)}, "b"},
		{"conflict beats critical path and priority", []Candidate{cand("a", func(c *Candidate) { c.Conflicts = 1; c.Height = 50; c.Task.Priority = 9 }), cand("b", nil)}, "b"},
		{"fewer conflicts by score", []Candidate{cand("a", func(c *Candidate) { c.Conflicts = 2 }), cand("b", func(c *Candidate) { c.Conflicts = 1 })}, "b"},
		{"conflicting beats nothing", []Candidate{cand("a", func(c *Candidate) { c.Conflicts = 1 })}, "a"},
		{"attempts", []Candidate{cand("a", func(c *Candidate) { c.Task.Attempts = 2 }), cand("b", nil)}, "b"},
		{"refail", []Candidate{cand("a", func(c *Candidate) { c.FailedBy = []string{"w1"}; c.Task.Priority = 2 }), cand("b", nil)}, "b"},
		{"refail by someone else", []Candidate{cand("a", func(c *Candidate) { c.FailedBy = []string{"w2"}; c.Task.Priority = 2 }), cand("b", nil)}, "a"},
		{"refail still beats nothing", []Candidate{cand("a", func(c *Candidate) { c.FailedBy = []string{"w1"} })}, "a"},
	}
	for _, tt := range tests {
		i := Pick(w, tt.cands, me)
		got := ""
		if i >= 0 {
			got = tt.cands[i].Task.ID
		}
		if got != tt.want {
			t.Errorf("%s: picked %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestScore(t *testing.T) {
	c := Candidate{
		Task:      graph.Task{ID: "x", Priority: 2, Caps: []string{"go"}, Files: []string{"a.go", "b.go"}, Attempts: 1},
		Height:    3,
		Conflicts: 1,
		FailedBy:  []string{"w"},
	}
	got, ok := Score(Defaults, c, Agent{ID: "w", Caps: []string{"go", "sql"}, Files: []string{"a.go"}})
	// 3*2 + 2*3 + 5*1 + 2*1 - 4*1 - 1*1 - 10
	if want := 4.0; !ok || got != want {
		t.Fatalf("Score = %v, %v; want %v", got, ok, want)
	}
}
