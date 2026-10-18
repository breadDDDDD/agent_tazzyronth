package graph

import (
	"slices"
	"strings"
	"testing"
)

func task(id string, deps ...string) Task { return Task{ID: id, Deps: deps} }

func state(t *testing.T, g *Graph, id string) State {
	t.Helper()
	tk, ok := g.Get(id)
	if !ok {
		t.Fatalf("task %q missing", id)
	}
	return tk.State
}

func TestAddValidation(t *testing.T) {
	tests := []struct {
		name    string
		before  []Task
		batch   []Task
		wantErr string
	}{
		{"empty id", nil, []Task{task("")}, "id is required"},
		{"duplicate in batch", nil, []Task{task("a"), task("a")}, "already exists"},
		{"duplicate existing", []Task{task("a")}, []Task{task("a")}, "already exists"},
		{"unknown dep", nil, []Task{task("a", "zz")}, "unknown dep"},
		{"self cycle", nil, []Task{task("a", "a")}, "cycle: a -> a"},
		{"two cycle", nil, []Task{task("a", "b"), task("b", "a")}, "cycle: a -> b -> a"},
		{"long cycle via batch", []Task{task("x")}, []Task{task("a", "c", "x"), task("b", "a"), task("c", "b")}, "cycle"},
		{"forward ref ok", nil, []Task{task("a", "b"), task("b")}, ""},
		{"diamond ok", []Task{task("r")}, []Task{task("a", "r"), task("b", "r"), task("c", "a", "b")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := New()
			if err := g.Add(tt.before...); err != nil {
				t.Fatal(err)
			}
			err := g.Add(tt.batch...)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Add: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Add err = %v, want %q", err, tt.wantErr)
			}
			// A rejected batch must leave the graph untouched.
			if got := len(g.List()); got != len(tt.before) {
				t.Fatalf("graph has %d tasks after rejected batch, want %d", got, len(tt.before))
			}
		})
	}
}

func TestReadinessPropagation(t *testing.T) {
	g := New()
	if err := g.Add(task("a"), task("b"), task("c", "a", "b"), task("d", "c")); err != nil {
		t.Fatal(err)
	}
	want := map[string]State{"a": Ready, "b": Ready, "c": Pending, "d": Pending}
	for id, s := range want {
		if got := state(t, g, id); got != s {
			t.Errorf("%s = %s, want %s", id, got, s)
		}
	}

	finish := func(id string) {
		t.Helper()
		for _, s := range []State{Claimed, Running, Done} {
			if err := g.SetState(id, s, "w1"); err != nil {
				t.Fatal(err)
			}
		}
	}
	finish("a")
	if got := state(t, g, "c"); got != Pending {
		t.Fatalf("c = %s after only a done, want pending", got)
	}
	finish("b")
	if got := state(t, g, "c"); got != Ready {
		t.Fatalf("c = %s after a,b done, want ready", got)
	}
	if got := state(t, g, "d"); got != Pending {
		t.Fatalf("d = %s, want pending", got)
	}

	// A task added later on top of done deps starts ready.
	if err := g.Add(task("e", "a")); err != nil {
		t.Fatal(err)
	}
	if got := state(t, g, "e"); got != Ready {
		t.Fatalf("e = %s, want ready", got)
	}
}

func TestTransitions(t *testing.T) {
	g := New()
	if err := g.Add(task("a"), task("b", "a")); err != nil {
		t.Fatal(err)
	}
	if err := g.SetState("b", Claimed, "w1"); err == nil {
		t.Error("claimed a pending task")
	}
	if err := g.SetState("a", Claimed, ""); err == nil {
		t.Error("claimed without owner")
	}
	if err := g.SetState("nope", Claimed, "w1"); err == nil {
		t.Error("claimed unknown task")
	}
	if err := g.SetState("a", Claimed, "w1"); err != nil {
		t.Fatal(err)
	}
	if tk, _ := g.Get("a"); tk.Owner != "w1" || tk.Attempts != 1 {
		t.Fatalf("after claim: owner %q attempts %d", tk.Owner, tk.Attempts)
	}
	if err := g.SetState("a", Ready, ""); err != nil {
		t.Fatal(err)
	}
	if tk, _ := g.Get("a"); tk.Owner != "" {
		t.Fatalf("owner %q kept after release", tk.Owner)
	}
	g.SetState("a", Claimed, "w2")
	g.SetState("a", Done, "")
	if err := g.SetState("a", Running, ""); err == nil {
		t.Error("left terminal state done")
	}
	if tk, _ := g.Get("a"); tk.Attempts != 2 || tk.Owner != "w2" {
		t.Fatalf("final: owner %q attempts %d", tk.Owner, tk.Attempts)
	}
}

func TestCriticalPath(t *testing.T) {
	g := New()
	// a -> b -> d -> e is the longest chain; c is a shortcut from a to e.
	err := g.Add(task("a"), task("b", "a"), task("c", "a"), task("d", "b"), task("e", "c", "d"), task("z"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := g.CriticalPath(), []string{"a", "b", "d", "e"}; !slices.Equal(got, want) {
		t.Fatalf("CriticalPath = %v, want %v", got, want)
	}
	h := g.Heights()
	if h["a"] != 4 || h["c"] != 2 || h["z"] != 1 {
		t.Fatalf("Heights = %v", h)
	}

	// Done work drops out of the remaining chain.
	for _, s := range []State{Claimed, Done} {
		g.SetState("a", s, "w")
	}
	if got, want := g.CriticalPath(), []string{"b", "d", "e"}; !slices.Equal(got, want) {
		t.Fatalf("CriticalPath after a done = %v, want %v", got, want)
	}
	if _, ok := g.Heights()["a"]; ok {
		t.Fatal("done task still has a height")
	}
}

func TestGetReturnsCopy(t *testing.T) {
	g := New()
	g.Add(task("a"), task("b", "a"))
	tk, _ := g.Get("b")
	tk.Deps[0] = "mutated"
	if again, _ := g.Get("b"); again.Deps[0] != "a" {
		t.Fatal("Get exposed internal state")
	}
}
