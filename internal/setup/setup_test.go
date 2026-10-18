package setup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tazzyronth/integrations"
)

const exe = `C:\Program Files\tazzy\tazzy.exe`

// testEnv is a fake machine rooted at a temp home: nothing on PATH unless
// listed, commands are recorded, never run.
func testEnv(t *testing.T, onPath ...string) (Env, *[][]string) {
	t.Helper()
	var ran [][]string
	return Env{
		Home:   t.TempDir(),
		Exe:    exe,
		Getenv: func(string) string { return "" },
		LookPath: func(name string) (string, error) {
			if slices.Contains(onPath, name) {
				return "/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		Run: func(argv []string) error { ran = append(ran, argv); return nil },
	}, &ran
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func applyAll(t *testing.T, e Env, hosts []Host) {
	t.Helper()
	for _, h := range hosts {
		if h.Err != nil {
			t.Fatalf("%s: %v", h.Name, h.Err)
		}
		for _, c := range h.Changes {
			if err := Apply(e, c); err != nil {
				t.Fatalf("%s: %v", c, err)
			}
		}
	}
}

func names(hosts []Host) []string {
	var n []string
	for _, h := range hosts {
		n = append(n, h.Name)
	}
	return n
}

func TestDetect(t *testing.T) {
	e, _ := testEnv(t, "codex", "agy")
	write(t, filepath.Join(e.Home, ".cursor", "argv.json"), "{}")
	if got := names(Plan(e)); !slices.Equal(got, []string{"codex", "antigravity", "cursor"}) {
		t.Errorf("detected %v", got)
	}
	if got := names(Plan(e, "gemini")); !slices.Equal(got, []string{"gemini"}) {
		t.Errorf("-host gemini gave %v", got)
	}
	empty, _ := testEnv(t)
	if got := Plan(empty); len(got) != 0 {
		t.Errorf("empty machine: %v", names(got))
	}
}

// TestInitIdempotent: every host on a machine with existing, unrelated
// config. Applying writes the right entries and keeps everything else; a
// second plan has nothing left to do.
func TestInitIdempotent(t *testing.T) {
	e, ran := testEnv(t, "claude", "codex", "gemini", "agy", "cursor")
	h := e.Home
	write(t, filepath.Join(h, ".codex", "config.toml"), "model = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"x\"\n")
	write(t, filepath.Join(h, ".gemini", "settings.json"), "\xef\xbb\xbf{\n  \"theme\": \"dark\",\n  \"mcpServers\": {\"other\": {\"command\": \"x\"}},\n  \"zeta\": [1, 2]\n}\n")
	write(t, filepath.Join(h, ".claude.json"), `{"numStartups": 3, "mcpServers": {}}`)

	hosts := Plan(e)
	if got := names(hosts); !slices.Equal(got, Names()) {
		t.Fatalf("detected %v", got)
	}
	applyAll(t, e, hosts)

	// Claude Code: the plugin is written out, nothing is run or installed;
	// the notes say how to install it and that tazzy must be on PATH.
	if len(*ran) != 0 {
		t.Errorf("ran %q", *ran)
	}
	plugin := filepath.Join(h, ClaudePluginDir)
	for _, f := range []string{".claude-plugin/plugin.json", ".claude-plugin/marketplace.json", ".mcp.json", "commands/tazzy.md", "hooks/hooks.json", "hooks/register.tsx", "types/index.d.ts"} {
		read(t, filepath.Join(plugin, f))
	}
	if _, err := os.Stat(filepath.Join(plugin, "tests")); err == nil {
		t.Error("plugin tests written out")
	}
	notes := strings.Join(hosts[0].Notes, " | ")
	if !strings.Contains(notes, "claude plugin marketplace add "+plugin) || !strings.Contains(notes, "claude plugin install tazzyronth@tazzyronth") ||
		!strings.Contains(notes, "PATH") || strings.Contains(notes, "claude mcp remove") {
		t.Errorf("claude notes: %s", notes)
	}

	// Codex: block appended after the existing config; skill in ~/.agents.
	toml := read(t, filepath.Join(h, ".codex", "config.toml"))
	want := "model = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"x\"\n\n[mcp_servers.tazzyronth]\ncommand = 'C:\\Program Files\\tazzy\\tazzy.exe'\nargs = [\"mcp\"]\n"
	if toml != want {
		t.Errorf("config.toml:\n%s\nwant:\n%s", toml, want)
	}
	if read(t, filepath.Join(h, ".agents", "skills", "tazzy", "SKILL.md")) != integrations.Skill {
		t.Error("skill not written")
	}

	// Gemini: one key added, the others kept in order.
	settings := read(t, filepath.Join(h, ".gemini", "settings.json"))
	if i, j, k := strings.Index(settings, `"theme"`), strings.Index(settings, `"mcpServers"`), strings.Index(settings, `"zeta"`); !(i >= 0 && i < j && j < k) {
		t.Errorf("key order lost:\n%s", settings)
	}
	var g struct {
		Theme      string
		Zeta       []int
		MCPServers map[string]struct {
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(settings), &g); err != nil || g.Theme != "dark" || len(g.Zeta) != 2 ||
		g.MCPServers["other"].Command != "x" || g.MCPServers["tazzyronth"].Command != exe || !slices.Equal(g.MCPServers["tazzyronth"].Args, []string{"mcp"}) {
		t.Errorf("settings.json = %s (%v)", settings, err)
	}
	if read(t, filepath.Join(h, ".gemini", "commands", "tazzy.toml")) != integrations.GeminiCommand {
		t.Error("gemini command not written")
	}

	// Antigravity: new MCP config; both skill dirs.
	var ag struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(h, ".gemini", "config", "mcp_config.json"))), &ag); err != nil || ag.MCPServers["tazzyronth"]["command"] != exe {
		t.Errorf("antigravity mcp_config.json: %v %v", ag, err)
	}
	for _, d := range []string{".gemini/antigravity-cli/skills", ".gemini/config/skills"} {
		read(t, filepath.Join(h, d, "tazzy", "SKILL.md"))
	}

	// Cursor: stdio entry.
	var cu struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(h, ".cursor", "mcp.json"))), &cu); err != nil || cu.MCPServers["tazzyronth"]["type"] != "stdio" {
		t.Errorf("cursor mcp.json: %v %v", cu, err)
	}

	// Second run: nothing to do. A user-scope server from an older init
	// would now be a duplicate: the notes say how to remove it.
	write(t, filepath.Join(h, ".claude.json"), `{"numStartups": 3, "mcpServers": {"tazzyronth": {"command": "x"}}}`)
	for _, host := range Plan(e) {
		if host.Err != nil || len(host.Changes) != 0 {
			t.Errorf("%s not idempotent: %v %v", host.Name, host.Changes, host.Err)
		}
		if host.Name == "claude-code" && !strings.Contains(strings.Join(host.Notes, " | "), "claude mcp remove --scope user tazzyronth") {
			t.Errorf("no duplicate-server note: %v", host.Notes)
		}
	}
}

func TestExistingEntriesKept(t *testing.T) {
	e, _ := testEnv(t, "codex", "cursor")
	custom := "{\"mcpServers\": {\"tazzyronth\": {\"command\": \"mine\", \"args\": [\"mcp\", \"-dir\", \"x\"]}}}"
	write(t, filepath.Join(e.Home, ".cursor", "mcp.json"), custom)
	toml := "[mcp_servers.\"tazzyronth\"]\ncommand = \"mine\"\n"
	write(t, filepath.Join(e.Home, ".codex", "config.toml"), toml)
	applyAll(t, e, Plan(e))
	if read(t, filepath.Join(e.Home, ".cursor", "mcp.json")) != custom || read(t, filepath.Join(e.Home, ".codex", "config.toml")) != toml {
		t.Error("an existing tazzyronth entry was rewritten")
	}
}

func TestBadJSONNotTouched(t *testing.T) {
	e, _ := testEnv(t, "gemini")
	bad := "{ // comments are not JSON\n}"
	path := filepath.Join(e.Home, ".gemini", "settings.json")
	write(t, path, bad)
	hosts := Plan(e)
	if len(hosts) != 1 || hosts[0].Err == nil {
		t.Fatalf("hosts = %+v", hosts)
	}
	if read(t, path) != bad {
		t.Error("bad config was modified")
	}
}

func TestCodexHome(t *testing.T) {
	e, _ := testEnv(t, "codex")
	alt := filepath.Join(e.Home, "alt-codex")
	e.Getenv = func(k string) string {
		if k == "CODEX_HOME" {
			return alt
		}
		return ""
	}
	applyAll(t, e, Plan(e))
	if !strings.Contains(read(t, filepath.Join(alt, "config.toml")), "[mcp_servers.tazzyronth]") {
		t.Error("CODEX_HOME ignored")
	}
}
