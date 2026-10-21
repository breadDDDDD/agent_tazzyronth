// Package setup is "tazzy init": it finds the agent hosts installed for the
// user and works out what to write so that each one has the tazzyronth MCP
// server and a /tazzy command (plan §7.1, §7.4). Planning only reads; Apply
// writes. Existing config is merged, never replaced: a JSON config gets one
// key under mcpServers, a TOML config gets a block appended if it has none.
package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"tazzyronth/integrations"
)

const server = "tazzyronth" // MCP server name in every host

// Env is the machine as setup sees it; tests use a temp Home and fakes.
type Env struct {
	Home     string
	Exe      string // absolute tazzy executable written into the configs
	Getenv   func(string) string
	LookPath func(string) (string, error)
	Run      func(argv []string) error // runs a host CLI (claude mcp add)
}

// Change is one file to write (Path, Data) or one command to run (Argv).
type Change struct {
	Path string
	Data []byte
	Argv []string
	Note string // what it does
}

func (c Change) String() string {
	if c.Argv != nil {
		return "run " + strings.Join(c.Argv, " ")
	}
	return c.Note + ": " + c.Path
}

// Host is one detected host and what it still needs. Empty Changes means it
// is set up already; Err means it could not be planned (nothing is written).
type Host struct {
	Name, Why string
	Changes   []Change
	Notes     []string // steps left to the user, shown with the plan
	Err       error
}

type hostDef struct {
	name string
	bins []string // any on PATH means installed
	dirs []string // ... or any of these under home
	plan func(e Env, p *planner)
}

var hosts = []hostDef{
	{name: "claude-code", bins: []string{"claude"}, dirs: []string{".claude"}, plan: planClaude},
	{name: "codex", bins: []string{"codex"}, dirs: []string{".codex"}, plan: planCodex},
	{name: "gemini", bins: []string{"gemini"}, dirs: []string{".gemini/settings.json", ".gemini/commands"}, plan: planGemini},
	{name: "antigravity", bins: []string{"agy", "antigravity"}, dirs: []string{".gemini/antigravity-cli", ".gemini/antigravity", ".gemini/config"}, plan: planAntigravity},
	{name: "cursor", bins: []string{"cursor", "cursor-agent"}, dirs: []string{".cursor"}, plan: planCursor},
}

// Names lists the hosts setup knows.
func Names() []string {
	n := make([]string, len(hosts))
	for i, h := range hosts {
		n[i] = h.name
	}
	return n
}

// Plan returns the detected hosts (all of them, set up or not), in a fixed
// order. only, if not empty, limits it to those names, detected or not.
func Plan(e Env, only ...string) []Host {
	var out []Host
	for _, d := range hosts {
		why := detect(e, d)
		if len(only) > 0 {
			if !slices.Contains(only, d.name) {
				continue
			}
			if why == "" {
				why = "requested"
			}
		}
		if why == "" {
			continue
		}
		p := &planner{e: e}
		d.plan(e, p)
		out = append(out, Host{Name: d.name, Why: why, Changes: p.changes, Notes: p.notes, Err: p.err})
	}
	return out
}

func detect(e Env, d hostDef) string {
	for _, b := range d.bins {
		if _, err := e.LookPath(b); err == nil {
			return b + " on PATH"
		}
	}
	for _, dir := range d.dirs {
		if _, err := os.Stat(filepath.Join(e.Home, dir)); err == nil {
			return "~/" + dir + " exists"
		}
	}
	return ""
}

// Apply makes one change.
func Apply(e Env, c Change) error {
	if c.Argv != nil {
		return e.Run(c.Argv)
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(c.Path, c.Data, 0o644)
}

// planner collects a host's changes, skipping ones already in place.
type planner struct {
	e       Env
	changes []Change
	notes   []string
	err     error
}

func (p *planner) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}

// file plans writing data to path unless it already holds exactly that.
func (p *planner) file(path, note string, data []byte) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return
	}
	p.changes = append(p.changes, Change{Path: path, Data: data, Note: note})
}

func (p *planner) skill(dir string) {
	p.file(filepath.Join(p.e.Home, dir, "tazzy", "SKILL.md"), "/tazzy skill", []byte(integrations.Skill))
}

// mcpJSON plans adding mcpServers.tazzyronth to a JSON config, keeping every
// other key (and their order) as it was.
func (p *planner) mcpJSON(path string, entry map[string]any) {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.fail(err)
		return
	}
	data, changed, err := mergeServer(old, entry)
	if err != nil {
		p.fail(fmt.Errorf("%s: %w (add the %q entry by hand)", path, err, server))
		return
	}
	if changed {
		p.changes = append(p.changes, Change{Path: path, Data: data, Note: "add mcpServers." + server})
	}
}

// allowJSON plans adding rule to permissions.allow in a JSON settings file.
func (p *planner) allowJSON(path, rule string) {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.fail(err)
		return
	}
	data, changed, err := mergeAllow(old, rule)
	if err != nil {
		p.fail(fmt.Errorf("%s: %w (add %q to permissions.allow by hand)", path, err, rule))
		return
	}
	if changed {
		p.changes = append(p.changes, Change{Path: path, Data: data, Note: "allow " + rule})
	}
}

func (p *planner) entry() map[string]any {
	return map[string]any{"command": p.e.Exe, "args": []string{"mcp"}}
}

// ClaudePluginDir is where init writes the Claude Code plugin (relative to
// home). It is also a one-plugin marketplace, so it installs with
// "claude plugin marketplace add" + "claude plugin install".
var ClaudePluginDir = filepath.Join(".tazzyronth", "claude-code")

// planClaude writes the Claude Code plugin (/tazzy, the MCP server, hooks
// and the UI mod) and says how to install it; installing is left to the
// user, as it changes Claude Code's own settings.
func planClaude(e Env, p *planner) {
	dir := filepath.Join(e.Home, ClaudePluginDir)
	err := fs.WalkDir(integrations.ClaudePlugin(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(integrations.ClaudePlugin(), path)
		if err == nil {
			p.file(filepath.Join(dir, filepath.FromSlash(path)), "Claude Code plugin", data)
		}
		return err
	})
	if err != nil {
		p.fail(err)
		return
	}
	p.notes = append(p.notes,
		"install the plugin (once): claude plugin marketplace add "+dir+"  then  claude plugin install tazzyronth@tazzyronth",
		"or try it for one session: claude --plugin-dir "+dir)
	// The plugin starts "tazzy mcp" and the hooks by name.
	if _, err := e.LookPath("tazzy"); err != nil {
		p.notes = append(p.notes, "put tazzy on PATH (the plugin runs `tazzy mcp` and `tazzy hook report`): "+e.Exe)
	}
	// An older init registered the server at user scope; with the plugin
	// installed it would be a second tazzyronth server.
	if b, err := os.ReadFile(filepath.Join(e.Home, ".claude.json")); err == nil {
		var cfg struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if json.Unmarshal(b, &cfg) == nil && cfg.MCPServers[server] != nil {
			p.notes = append(p.notes, "the plugin brings its own MCP server: remove the old one with `claude mcp remove --scope user "+server+"`")
		}
	}
}

func planCodex(e Env, p *planner) {
	home := e.Getenv("CODEX_HOME")
	if home == "" {
		home = filepath.Join(e.Home, ".codex")
	}
	path := filepath.Join(home, "config.toml")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.fail(err)
		return
	}
	if !hasTOMLTable(string(old), "mcp_servers."+server) {
		block := fmt.Sprintf("[mcp_servers.%s]\ncommand = %s\nargs = [\"mcp\"]\n", server, tomlString(e.Exe))
		data := string(old)
		if data != "" {
			data = strings.TrimRight(data, "\r\n") + "\n\n"
		}
		p.changes = append(p.changes, Change{Path: path, Data: []byte(data + block), Note: "append [mcp_servers." + server + "]"})
	}
	// Custom prompts (~/.codex/prompts) are deprecated in favour of skills;
	// ~/.agents/skills is Codex's user skill dir (Cursor reads it too).
	p.skill(".agents/skills")
}

func planGemini(e Env, p *planner) {
	p.mcpJSON(filepath.Join(e.Home, ".gemini", "settings.json"), p.entry())
	p.file(filepath.Join(e.Home, ".gemini", "commands", "tazzy.toml"), "/tazzy command", []byte(integrations.GeminiCommand))
}

// AntigravityAllow is the Antigravity CLI permission rule for the tazzyronth
// tools: headless agy (hub-spawned workers) soft-denies MCP calls without one.
const AntigravityAllow = "mcp(" + server + "/*)"

func planAntigravity(e Env, p *planner) {
	// One MCP config for the CLI and the IDE, also used by hub-spawned agy
	// workers (agy has no per-run MCP config flag). The skill dirs differ:
	// https://antigravity.google/docs/skills/. Workflows (.agents/workflows)
	// stop working on 2026-10-19, so none is written.
	p.mcpJSON(filepath.Join(e.Home, ".gemini", "config", "mcp_config.json"), p.entry())
	p.skill(".gemini/antigravity-cli/skills")
	p.skill(".gemini/config/skills")
	// https://antigravity.google/docs/permissions?tab=cli (the IDE and the
	// app keep permissions in their own settings UI).
	p.allowJSON(filepath.Join(e.Home, ".gemini", "antigravity-cli", "settings.json"), AntigravityAllow)
}

func planCursor(e Env, p *planner) {
	entry := p.entry()
	entry["type"] = "stdio"
	p.mcpJSON(filepath.Join(e.Home, ".cursor", "mcp.json"), entry)
	// Cursor also reads ~/.agents/skills: one copy there serves Codex and
	// Cursor, and a second in ~/.cursor/skills would show up twice.
	p.skill(".agents/skills")
}
