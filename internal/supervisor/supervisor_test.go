package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"tazzyronth/internal/config"
)

// The test binary doubles as the worker: TAZZY_HELPER=parent starts a
// grandchild (TAZZY_HELPER=sleep), writes its pid to child.pid and sleeps.
func init() {
	switch os.Getenv("TAZZY_HELPER") {
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "parent":
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "TAZZY_HELPER=sleep")
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		os.WriteFile("child.pid", []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
		time.Sleep(time.Minute)
		os.Exit(0)
	case "exit":
		os.Exit(7)
	}
}

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false // Windows: no such process
	}
	defer p.Release()
	if runtime.GOOS == "windows" {
		return true
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func spawn(t *testing.T, helper string) (dir string, s *Supervisor, exits chan error) {
	t.Helper()
	dir = t.TempDir()
	exits = make(chan error, 1)
	s = New(dir, "tazzy-test", func(agent string, err error) {
		if agent != "w1" {
			t.Errorf("exit of %q", agent)
		}
		exits <- err
	})
	t.Cleanup(s.Close)
	t.Setenv("TAZZY_HELPER", helper)
	a := config.Adapter{Cmd: []string{os.Args[0], "{agent_id}"}, Env: map[string]string{"TAZZY_CFG": "{mcp_config}"}}
	if err := s.Spawn("w1", a, "do {things}", ""); err != nil {
		t.Fatal(err)
	}
	return dir, s, exits
}

func TestExitObserved(t *testing.T) {
	dir, s, exits := spawn(t, "exit")
	select {
	case err := <-exits:
		if err == nil || !strings.Contains(err.Error(), "exit status 7") {
			t.Errorf("exit err = %v, want exit status 7", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("exit not observed")
	}
	if s.Running() || len(s.Live()) != 0 {
		t.Error("worker still listed after exit")
	}
	run := filepath.Join(dir, ".tazzyronth", "run", "w1")
	if b, err := os.ReadFile(filepath.Join(run, "prompt.md")); err != nil || string(b) != "do {things}" {
		t.Errorf("prompt.md = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(run, "mcp.json")); err != nil || !strings.Contains(string(b), `"tazzy-test"`) {
		t.Errorf("mcp.json = %s, %v", b, err)
	}
}

// TestKillTree: Kill takes the worker's children with it.
func TestKillTree(t *testing.T) {
	dir, s, exits := spawn(t, "parent")
	pidFile := filepath.Join(dir, "child.pid")
	var child int
	for deadline := time.Now().Add(10 * time.Second); child == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("grandchild never started")
		}
		b, _ := os.ReadFile(pidFile)
		child, _ = strconv.Atoi(string(b))
	}
	if !alive(child) {
		t.Fatal("alive(grandchild) = false before Kill")
	}
	if got := s.Live(); len(got) != 1 || got[0] != "w1" {
		t.Fatalf("Live = %v", got)
	}
	if !s.Kill("w1") {
		t.Fatal("Kill = false")
	}
	if len(s.Live()) != 0 {
		t.Error("killed worker still live")
	}
	select {
	case <-exits:
	case <-time.After(10 * time.Second):
		t.Fatal("exit not observed")
	}
	for deadline := time.Now().Add(5 * time.Second); alive(child); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived", child)
		}
	}
}

func TestPrompt(t *testing.T) {
	p := Prompt("a7", "/proj", "ship it")
	for _, want := range []string{`agent="a7"`, "/proj", "ship it", "tazzy_next", "tazzy_lease", "tazzy_done", "tazzy_wait"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "{") {
		t.Errorf("unexpanded placeholder in prompt:\n%s", p)
	}
}

// TestNoRealCLIs: tests can never launch a real LLM CLI, whatever the config.
func TestNoRealCLIs(t *testing.T) {
	s := New(t.TempDir(), "tazzy", func(string, error) { t.Error("something was started") })
	defer s.Close()
	for _, name := range []string{"claude", "codex", `C:\npm\gemini.cmd`} {
		if err := s.Spawn("w", config.Adapter{Cmd: []string{name, "-p", "{prompt}"}}, "p", ""); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
