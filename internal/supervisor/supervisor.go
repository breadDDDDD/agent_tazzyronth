// Package supervisor launches hub-spawned workers (plan §2.4 mode A, §3.8)
// from adapter templates and reports when they exit. Each worker runs in the
// project directory with its own run dir (.tazzyronth/run/<agent>/: MCP
// config and bootstrap prompt) and log (.tazzyronth/logs/<agent>.log). A
// worker's whole process tree is killed with it, and with the hub: a Job
// Object on Windows, a process group on Unix.
package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tazzyronth/internal/config"
	"tazzyronth/playbooks"
)

// Prompt is the bootstrap prompt for a worker: who it is, then the worker
// playbook, then the adapter's note (config.Adapter.Note) if any.
func Prompt(agent, workdir, goal, note string) string {
	if goal == "" {
		goal = "(see tazzy_join)"
	}
	p := fmt.Sprintf("You are worker %s in an Agent Tazzyronth swarm, working in %s. Your agent id is %q: pass agent=%q on every tazzy_* call.\nSwarm goal: %s\n\n",
		agent, workdir, agent, agent, goal) + playbooks.Worker
	if note != "" {
		p += "\n" + note + "\n"
	}
	return p
}

// Supervisor owns the worker processes. Its methods are safe for concurrent use.
type Supervisor struct {
	root   string // absolute project dir
	exe    string // the tazzy executable workers' MCP configs run
	onExit func(agent string, err error)
	stdout func(agent, format string) io.WriteCloser

	mu    sync.Mutex
	procs map[string]*proc
	wg    sync.WaitGroup
}

type proc struct {
	tree   tree
	killed bool
}

// New returns a supervisor for the project at root. onExit is called (from
// another goroutine, with no supervisor lock held) once per spawned worker
// when its process exits; err is nil for exit status 0.
func New(root, exe string, onExit func(agent string, err error)) *Supervisor {
	return &Supervisor{root: root, exe: exe, onExit: onExit, procs: map[string]*proc{}}
}

// SetStdout makes Spawn copy the stdout of workers whose adapter declares a
// stream format to f's writer, as well as to the log. The writer is closed
// once the worker has exited; it must never block for long, or the worker
// stalls on a full pipe.
func (s *Supervisor) SetStdout(f func(agent, format string) io.WriteCloser) { s.stdout = f }

func (s *Supervisor) stateDir(parts ...string) string {
	return filepath.Join(append([]string{s.root, ".tazzyronth"}, parts...)...)
}

// Spawn starts a worker for agent (an id already registered with the hub)
// in workdir ("" = the project dir; a worktree in worktree isolation).
func (s *Supervisor) Spawn(agent string, a config.Adapter, prompt, workdir string) error {
	if workdir == "" {
		workdir = s.root
	}
	if len(a.Cmd) == 0 {
		return errors.New("adapter has no cmd")
	}
	if testing.Testing() && realCLI(a.Cmd[0]) {
		return fmt.Errorf("refusing to launch %s from a test: use a fake adapter", a.Cmd[0])
	}
	s.mu.Lock()
	_, dup := s.procs[agent]
	s.mu.Unlock()
	if dup {
		return fmt.Errorf("worker %s is already running", agent)
	}

	run := s.stateDir("run", agent)
	if err := os.MkdirAll(run, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(s.stateDir("logs"), 0o700); err != nil {
		return err
	}
	mcpConfig := filepath.Join(run, "mcp.json")
	promptFile := filepath.Join(run, "prompt.md")
	cfg, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{
		"tazzyronth": map[string]any{"command": s.exe, "args": []string{"mcp", "-dir", s.root, "-agent", agent}},
	}}, "", "  ")
	if err := os.WriteFile(mcpConfig, cfg, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(promptFile, []byte(prompt), 0o600); err != nil {
		return err
	}

	r := strings.NewReplacer(
		"{prompt}", prompt, "{prompt_file}", promptFile, "{mcp_config}", mcpConfig,
		"{agent_id}", agent, "{workdir}", workdir, "{project}", s.root, "{tazzy}", s.exe)
	argv := make([]string, len(a.Cmd))
	for i, arg := range a.Cmd {
		argv[i] = r.Replace(arg)
	}
	env := append(os.Environ(), "TAZZY_AGENT_ID="+agent)
	for _, k := range slices.Sorted(maps.Keys(a.Env)) {
		env = append(env, k+"="+r.Replace(a.Env[k]))
	}

	logf, err := os.OpenFile(s.stateDir("logs", agent+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = workdir, env
	cmd.Stdout, cmd.Stderr = logf, logf // stdin: the null device
	var tee io.WriteCloser
	if a.Stream != "" && s.stdout != nil {
		tee = s.stdout(agent, a.Stream)
		cmd.Stdout = io.MultiWriter(logf, tee)
		// A grandchild that inherited stdout mustn't hold Wait (and so the
		// tree kill that follows it) open.
		cmd.WaitDelay = 2 * time.Second
	}
	closeTee := func() {
		if tee != nil {
			tee.Close()
		}
	}
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		logf.Close()
		closeTee()
		return err
	}
	t, err := newTree(cmd.Process)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		logf.Close()
		closeTee()
		return fmt.Errorf("contain worker process: %w", err)
	}

	s.mu.Lock()
	s.procs[agent] = &proc{tree: t}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		err := cmd.Wait()
		logf.Close()
		closeTee()
		s.mu.Lock()
		delete(s.procs, agent)
		s.mu.Unlock()
		t.close() // takes any leftover children (e.g. its tazzy mcp) with it
		if s.onExit != nil {
			s.onExit(agent, err)
		}
	}()
	return nil
}

// Kill terminates the worker's process tree. It reports whether there was a
// live worker; its exit is reported through onExit as usual.
func (s *Supervisor) Kill(agent string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.procs[agent]
	if p == nil || p.killed {
		return false
	}
	p.killed = true
	p.tree.kill()
	return true
}

// Live lists the workers that are running and not being killed, sorted.
func (s *Supervisor) Live() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, p := range s.procs {
		if !p.killed {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// Running reports whether any worker process (killed or not) has yet to exit.
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.procs) > 0
}

// Close kills every worker and waits for all exits to be reported.
func (s *Supervisor) Close() {
	s.mu.Lock()
	for _, p := range s.procs {
		p.killed = true
		p.tree.kill()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// realCLI reports whether cmd is one of the LLM CLIs the built-in adapters
// run, which tests must never launch (they cost money and touch the repo).
func realCLI(cmd string) bool {
	name := strings.ToLower(strings.TrimSuffix(filepath.Base(cmd), filepath.Ext(cmd)))
	return name == "claude" || name == "codex" || name == "gemini" || name == "agy" || name == "antigravity"
}

// Has reports whether agent's worker process (killed or not) has yet to exit.
func (s *Supervisor) Has(agent string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[agent] != nil
}
