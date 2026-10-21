package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(Path(dir)), 0o700)
	if err := os.WriteFile(Path(dir), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir)
	if err != nil || c.MaxAgents != 5 || c.SpawnMode != SpawnAuto || c.Isolation != IsolationLease || len(c.Adapters["claude"].Cmd) == 0 ||
		c.Timeouts.QuickExit != 60 || c.Budget.MaxQuickExits != 3 {
		t.Fatalf("defaults: %+v, %v", c, err)
	}

	write(t, dir, `{"max_agents": 2, "timeouts": {"idle_retire_s": 0.5}, "budget": {"max_quick_exits": 0},
		"scheduler": {"weights": {"conflict": 9}},
		"adapters": {"mine": {"cmd": ["my-agent", "{prompt}"]}}}`)
	c, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxAgents != 2 || c.Timeouts.IdleRetire != 0.5 || c.Timeouts.ScaleTick != 2 ||
		c.Budget.MaxQuickExits != 0 || c.Budget.MaxTotalSpawns != 20 || c.Timeouts.QuickExit != 60 {
		t.Errorf("override/merge: %+v", c)
	}
	if w := c.Scheduler.Weights; w.Conflict != 9 || w.Priority != 3 {
		t.Errorf("weights: %+v", w)
	}
	if len(c.Adapters["mine"].Cmd) != 2 || len(c.Adapters["claude"].Cmd) == 0 {
		t.Errorf("adapters: %v", c.Adapters)
	}

	for _, bad := range []string{`{"max_agents": 0}`, `{"min_agents": 9}`, `{"spawn_mode": "x"}`, `{"isolation": "x"}`, `{"adapters": {"e": {}}}`,
		`{"timeouts": {"quick_exit_s": -1}}`, `{"budget": {"max_quick_exits": -1}}`, `{`} {
		write(t, dir, bad)
		if _, err := Load(dir); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

// TestNotes: only antigravity carries a prompt note; a user override of it
// replaces the built-in whole, note included.
func TestNotes(t *testing.T) {
	for name, a := range Default().Adapters {
		if (a.Note != "") != (name == "antigravity") {
			t.Errorf("%s: note %q", name, a.Note)
		}
	}
	dir := t.TempDir()
	write(t, dir, `{"adapters": {"antigravity": {"cmd": ["agy", "-p", "{prompt}"]}, "mine": {"cmd": ["x"], "note": "be careful"}}}`)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Adapters["antigravity"].Note != "" || c.Adapters["mine"].Note != "be careful" {
		t.Errorf("notes after override: %+v", c.Adapters)
	}
}

func TestWorkerAdapter(t *testing.T) {
	c := Default()
	cases := []struct{ explicit, def, client, want string }{
		{"", "", "codex-mcp-client", "codex"},
		{"", "", "gemini-cli-mcp-client", "gemini"},
		{"", "", "antigravity-client", "antigravity"}, // agy 1.4.2's clientInfo.name
		{"", "", "claude-code", "claude"},
		{"", "", "cursor-vscode", FallbackAdapter}, // no adapter for this host
		{"", "", "", FallbackAdapter},
		{"", "fake", "codex-mcp-client", "fake"}, // default_adapter set in config wins
		{"gemini", "fake", "codex-mcp-client", "gemini"},
	}
	for _, tc := range cases {
		c.DefaultAdapter = tc.def
		if got := c.WorkerAdapter(tc.explicit, tc.client); got != tc.want {
			t.Errorf("WorkerAdapter(%q, %q) with default %q = %q, want %q", tc.explicit, tc.client, tc.def, got, tc.want)
		}
	}
	delete(c.Adapters, "codex")
	c.DefaultAdapter = ""
	if got := c.WorkerAdapter("", "codex"); got != FallbackAdapter {
		t.Errorf("codex not configured: got %q", got)
	}
}
