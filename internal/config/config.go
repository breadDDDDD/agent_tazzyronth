// Package config loads .tazzyronth/config.json over built-in defaults
// (plan §8). Only the fields something actually reads are here.
package config

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tazzyronth/internal/sched"
)

// Spawn modes (plan §2.4).
const (
	SpawnHub  = "hub"  // the hub launches workers through adapters and scales them
	SpawnHost = "host" // the lead's host spawns its own subagents; the hub only coordinates
	SpawnAuto = "auto" // host if the lead's MCP client has native subagents, else hub
)

// Isolation modes (plan §3.4).
const (
	IsolationLease    = "lease"    // agents share the checkout; leases keep them apart
	IsolationWorktree = "worktree" // each agent works in its own git worktree and branch
)

// ResolveSpawn turns a spawn mode into hub or host. auto picks host for MCP
// clients known to have native subagents the lead starts itself (Claude
// Code's Agent tool) and hub for every other client, so a plain /tazzy does
// work anywhere. With no client known (no lead yet) nothing is spawned.
func ResolveSpawn(mode, client string) string {
	if mode != SpawnAuto {
		return mode
	}
	if client == "" || strings.Contains(strings.ToLower(client), "claude") {
		return SpawnHost
	}
	return SpawnHub
}

// Adapter launches one worker CLI. Cmd and Env values may use the
// placeholders {prompt}, {prompt_file}, {mcp_config}, {agent_id}, {workdir},
// {tazzy} (this executable) and {project}.
type Adapter struct {
	Cmd []string          `json:"cmd"`
	Env map[string]string `json:"env,omitempty"`
	// Stream is the format of the JSON event stream the worker prints on
	// stdout (activity.Formats: claude, codex, gemini, antigravity); "" = none.
	Stream string `json:"stream,omitempty"`
	// Note is extra advice for this CLI appended to the worker's bootstrap
	// prompt after the playbook; "" = none. A user adapter replaces the
	// built-in of the same name whole, so an override without a note has none.
	Note string `json:"note,omitempty"`
}

// Activity configures activity capture (plan §3.9).
type Activity struct {
	Ring  int     `json:"ring"`    // events kept in memory per agent
	Poll  float64 `json:"poll_s"`  // how often leased files are checked for edits
	Sweep float64 `json:"sweep_s"` // how often git status is checked for unleased edits; 0 = never
}

type Timeouts struct {
	IdleRetire    float64 `json:"idle_retire_s"`    // a hub-spawned worker idle this long is retired (if not needed)
	SpawnCooldown float64 `json:"spawn_cooldown_s"` // minimum gap between automatic spawns
	ScaleTick     float64 `json:"scale_tick_s"`     // how often the scaler runs
	HubIdle       float64 `json:"hub_idle_s"`       // the hub exits (killing its workers) after this long with no host connected
	Stall         float64 `json:"stall_s"`          // an agent holding work that is silent this long is pinged
	StallGrace    float64 `json:"stall_grace_s"`    // ... and treated as dead if still silent this long after the ping
	FinishGrace   float64 `json:"finish_grace_s"`   // once every task has ended for this long, the workers are released
	QuickExit     float64 `json:"quick_exit_s"`     // a hub-spawned worker that dies within this long of its spawn died quickly (crash guard)
}

// Retries is the failure policy (plan §3.7).
type Retries struct {
	MaxAttempts   int `json:"max_attempts"`   // claims per task before it fails for good; a task's own max_attempts overrides
	EscalateAfter int `json:"escalate_after"` // tell the lead once a task has failed on this many different agents
}

type Budget struct {
	MaxTotalSpawns int `json:"max_total_spawns"` // over the whole swarm, including replays
	// MaxQuickExits is how many hub-spawned workers in a row may die within
	// quick_exit_s of starting before the hub pauses the swarm; 0 = never.
	MaxQuickExits int `json:"max_quick_exits"`
}

type Config struct {
	MaxAgents      int                `json:"max_agents"` // hub-spawned workers alive at once
	MinAgents      int                `json:"min_agents"`
	DefaultAdapter string             `json:"default_adapter"` // "": match the lead's host, else claude
	SpawnMode      string             `json:"spawn_mode"`      // hub | host | auto; tazzy_start(spawn) overrides
	Isolation      string             `json:"isolation"`       // lease | worktree; tazzy_start(isolation) overrides
	Timeouts       Timeouts           `json:"timeouts"`
	Budget         Budget             `json:"budget"`
	Retries        Retries            `json:"retries"`
	Scheduler      Scheduler          `json:"scheduler"`
	Activity       Activity           `json:"activity"`
	TUI            TUI                `json:"tui"`
	Web            bool               `json:"web"`      // serve the web view (plan §6.4) from the start
	Adapters       map[string]Adapter `json:"adapters"` // merged over the built-ins by name
}

// TUI configures the dashboard (plan §7.2).
type TUI struct {
	// Open is where tazzy_start opens it: auto (default), split, tab, window,
	// web (the browser) or none. auto means web in an IDE terminal.
	Open string `json:"open,omitempty"`
}

type Scheduler struct {
	Weights sched.Weights `json:"weights"`
}

// Built-in adapters. Every worker gets an MCP config with a single server,
// "tazzyronth", that runs "tazzy mcp -dir <project> -agent <id>", and is asked
// for a JSON event stream on stdout (parsed in Phase 6).
var builtins = map[string]Adapter{
	// Verified against `claude --help` (2026-10). stream-json needs --verbose
	// with -p; the MCP tools must be allowed explicitly or -p denies them.
	"claude": {Cmd: []string{"claude", "-p", "{prompt}",
		"--mcp-config", "{mcp_config}", "--strict-mcp-config",
		"--allowedTools", "mcp__tazzyronth",
		"--permission-mode", "acceptEdits",
		"--output-format", "stream-json", "--verbose"},
		Stream: "claude"},
	// Unverified (codex not installed): codex has no MCP config file flag, so
	// the server is injected with -c TOML overrides.
	"codex": {Cmd: []string{"codex", "exec", "--json", "--full-auto", "--skip-git-repo-check",
		"-C", "{workdir}",
		"-c", "mcp_servers.tazzyronth.command='{tazzy}'",
		"-c", "mcp_servers.tazzyronth.args=['mcp','-dir','{project}','-agent','{agent_id}']",
		"{prompt}"},
		Stream: "codex"},
	// Unverified (gemini not installed): the generated MCP config doubles as a
	// system settings file ({"mcpServers": ...} is the same shape).
	"gemini": {Cmd: []string{"gemini", "-p", "{prompt}",
		"--output-format", "stream-json",
		"--approval-mode", "auto_edit",
		"--allowed-mcp-server-names", "tazzyronth"},
		Env:    map[string]string{"GEMINI_CLI_SYSTEM_SETTINGS_PATH": "{mcp_config}"},
		Stream: "gemini"},
	// Verified against agy 1.4.2 (`agy --help`, a real -p run): agy has no
	// per-run MCP config flag, so the worker uses the global "tazzyronth"
	// entry `tazzy init` writes (~/.gemini/config/mcp_config.json); that
	// `tazzy mcp` takes the project and agent from TAZZY_PROJECT and
	// TAZZY_AGENT_ID. Headless agy soft-denies MCP calls without an
	// mcp(tazzyronth/*) allow rule (also written by init) and shell commands
	// unless allowed in ~/.gemini/antigravity-cli/settings.json.
	"antigravity": {Cmd: []string{"agy", "-p", "{prompt}",
		"--output-format", "stream-json",
		"--mode", "accept-edits",
		"--print-timeout", "0s"},
		Env:    map[string]string{"TAZZY_PROJECT": "{project}"},
		Stream: "antigravity",
		// One denied action ends a headless agy run at once (agy 1.4.2).
		Note: "You run headless: any action not on the allow list ends your run at once and loses your work. " +
			"Inspect and edit files only with your file tools (list, read, write, edit), never shell commands like Get-ChildItem, Test-Path, cat or ls. " +
			"Run a shell command only when the task spec names it (e.g. go test) and it is expected to be allowed; " +
			"do not run code to check your work (e.g. python -c, node -e) unless the spec names that command."},
}

// Default returns the configuration used when there is no config file.
func Default() Config {
	c := Config{
		MaxAgents: 5,
		SpawnMode: SpawnAuto,
		Isolation: IsolationLease,
		Timeouts:  Timeouts{IdleRetire: 90, SpawnCooldown: 5, ScaleTick: 2, HubIdle: 30, Stall: 300, StallGrace: 60, FinishGrace: 30, QuickExit: 60},
		Budget:    Budget{MaxTotalSpawns: 20, MaxQuickExits: 3},
		Retries:   Retries{MaxAttempts: 3, EscalateAfter: 2},
		Scheduler: Scheduler{Weights: sched.Defaults},
		Activity:  Activity{Ring: 500, Poll: 1, Sweep: 5},
		Adapters:  map[string]Adapter{},
	}
	for k, v := range builtins {
		c.Adapters[k] = v
	}
	return c
}

// Path is where a project's config lives.
func Path(projectDir string) string {
	return filepath.Join(projectDir, ".tazzyronth", "config.json")
}

// Load reads the project's config over the defaults; a missing file means
// defaults. Fields left out of the file keep their default values.
func Load(projectDir string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(Path(projectDir))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", Path(projectDir), err)
	}
	if err := c.validate(); err != nil {
		return c, fmt.Errorf("%s: %w", Path(projectDir), err)
	}
	return c, nil
}

func (c Config) validate() error {
	switch {
	case c.MaxAgents < 1:
		return errors.New("max_agents must be at least 1")
	case c.MinAgents < 0 || c.MinAgents > c.MaxAgents:
		return errors.New("min_agents must be between 0 and max_agents")
	case c.SpawnMode != SpawnHub && c.SpawnMode != SpawnHost && c.SpawnMode != SpawnAuto:
		return fmt.Errorf("spawn_mode must be %q, %q or %q", SpawnHub, SpawnHost, SpawnAuto)
	case c.Isolation != IsolationLease && c.Isolation != IsolationWorktree:
		return fmt.Errorf("isolation must be %q or %q", IsolationLease, IsolationWorktree)
	case c.Timeouts.ScaleTick <= 0:
		return errors.New("timeouts.scale_tick_s must be positive")
	case c.Timeouts.Stall <= 0 || c.Timeouts.StallGrace <= 0:
		return errors.New("timeouts.stall_s and stall_grace_s must be positive")
	case c.Timeouts.FinishGrace < 0:
		return errors.New("timeouts.finish_grace_s must not be negative")
	case c.Activity.Ring < 1 || c.Activity.Poll <= 0 || c.Activity.Sweep < 0:
		return errors.New("activity: ring must be at least 1, poll_s positive and sweep_s not negative")
	case c.Timeouts.QuickExit < 0 || c.Budget.MaxQuickExits < 0:
		return errors.New("timeouts.quick_exit_s and budget.max_quick_exits must not be negative")
	case c.Retries.MaxAttempts < 1:
		return errors.New("retries.max_attempts must be at least 1")
	}
	for name, a := range c.Adapters {
		if len(a.Cmd) == 0 {
			return fmt.Errorf("adapter %q: cmd is empty", name)
		}
	}
	return nil
}

// Seconds converts a config duration.
func Seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// FallbackAdapter is the worker adapter when nothing else picks one.
const FallbackAdapter = "claude"

// WorkerAdapter picks the adapter for hub-spawned workers: explicit (the
// lead's choice), else default_adapter, else the one matching the lead's MCP
// client name (codex, gemini, claude) if configured, else FallbackAdapter.
func (c Config) WorkerAdapter(explicit, client string) string {
	if a := cmp.Or(explicit, c.DefaultAdapter); a != "" {
		return a
	}
	client = strings.ToLower(client)
	for _, name := range []string{"codex", "antigravity", "gemini", "claude"} {
		if _, ok := c.Adapters[name]; ok && strings.Contains(client, name) {
			return name
		}
	}
	return FallbackAdapter
}
