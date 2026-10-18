package main

import (
	"bytes"
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"tazzyronth/internal/config"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/launch"
	"tazzyronth/internal/setup"
)

// runDoctor checks the local environment. It reads files, looks at PATH,
// runs git --version and dials the project's hub on loopback; nothing else.
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dir := fs.String("dir", ".", "project directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return doctor(os.Stdout, *dir)
}

func doctor(w io.Writer, dir string) int {
	problems := 0
	report := func(ok bool, name, format string, a ...any) {
		mark := "ok  "
		if !ok {
			mark = "warn"
			problems++
		}
		fmt.Fprintf(w, "[%s] %-14s %s\n", mark, name, fmt.Sprintf(format, a...))
	}
	info := func(name, format string, a ...any) {
		fmt.Fprintf(w, "[info] %-14s %s\n", name, fmt.Sprintf(format, a...))
	}

	fmt.Fprintf(w, "tazzy %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	abs, _ := filepath.Abs(dir)
	fmt.Fprintf(w, "project %s\n\n", abs)

	// tazzy itself: hosts and hooks start it by name.
	if p, err := exec.LookPath("tazzy"); err == nil {
		report(true, "tazzy on PATH", "%s", p)
	} else {
		exe, _ := os.Executable()
		report(false, "tazzy on PATH", "not found; the Claude Code plugin and hooks run `tazzy` by name (this binary: %s)", exe)
	}

	// Config.
	cfg, err := config.Load(dir)
	if err != nil {
		report(false, "config", "%v (using defaults)", err)
		cfg = config.Default()
	} else if _, statErr := os.Stat(config.Path(dir)); statErr == nil {
		report(true, "config", "%s", config.Path(dir))
	} else {
		info("config", "no .tazzyronth/config.json; using defaults")
	}

	// Hub.
	hi, err := hub.ReadInfo(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		info("hub", "not running (starts on the first tazzy tool call)")
	case err != nil:
		report(false, "hub", "unreadable hub.json: %v", err)
	default:
		if c, err := hub.Dial(hi.Addr(), hi.Token); err == nil {
			c.Close()
			web := ""
			if hi.Web != 0 {
				web = fmt.Sprintf(", web view on http://127.0.0.1:%d/", hi.Web)
			}
			report(true, "hub", "running on %s (pid %d)%s", hi.Addr(), hi.PID, web)
		} else {
			report(false, "hub", "stale hub.json (pid %d, port %d not answering: %v); the next start overwrites it", hi.PID, hi.Port, err)
		}
	}

	// Adapters: the worker CLIs a hub-spawned swarm would run.
	names := make([]string, 0, len(cfg.Adapters))
	for n := range cfg.Adapters {
		names = append(names, n)
	}
	slices.Sort(names)
	var found []string
	for _, n := range names {
		bin := cfg.Adapters[n].Cmd[0]
		if p, err := exec.LookPath(bin); err == nil {
			found = append(found, n)
			info("adapter "+n, "%s", p)
		} else {
			info("adapter "+n, "%s not installed", bin)
		}
	}
	if len(found) == 0 {
		report(false, "adapters", "no worker CLI installed; hub spawn mode cannot start workers (host mode still works)")
	}
	worker := cfg.WorkerAdapter("", "")
	if !slices.Contains(found, worker) {
		info("default worker", "%s (not installed)", worker)
	} else {
		info("default worker", "%s", worker)
	}

	// Git, for isolation "worktree".
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		report(cfg.Isolation != config.IsolationWorktree, "git", "not found (needed only for isolation \"worktree\")")
	} else {
		v := strings.TrimSpace(string(out))
		ok := gitAtLeast(v, 2, 38)
		if ok {
			report(true, "git", "%s", v)
		} else {
			report(cfg.Isolation != config.IsolationWorktree, "git", "%s; worktree isolation needs 2.38+", v)
		}
		top, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
		switch {
		case err != nil:
			info("repo", "not a git repo (worktree isolation unavailable; lease mode is fine)")
		case !samePath(strings.TrimSpace(string(top)), abs):
			info("repo", "project is inside %s; worktree isolation needs the repo top level", strings.TrimSpace(string(top)))
		case exec.Command("git", "-C", dir, "rev-parse", "--verify", "-q", "HEAD").Run() != nil:
			info("repo", "git repo with no commits yet; worktree isolation needs one")
		default:
			info("repo", "git repo top level; worktree isolation available")
		}
	}
	info("isolation", "%s", cfg.Isolation)

	// Terminal and dashboard placement.
	tc := useColor(os.Stdout)
	switch {
	case tc:
		info("terminal", "truecolor")
	case os.Getenv("NO_COLOR") != "":
		info("terminal", "colour off (NO_COLOR)")
	default:
		info("terminal", "no truecolor detected (stdout not a terminal, TERM=dumb, or COLORTERM unset); plain output")
	}
	env := launch.System()
	mode := cfg.TUI.Open
	if launch.WantsWeb(env, mode) {
		info("dashboard", "web view in the default browser (tui.open %q)", cmp.Or(mode, "auto"))
	} else {
		p := launch.Decide(env, mode, []string{"tazzy", "tui"})
		if p.Argv == nil {
			info("dashboard", "no launcher: %s; run `tazzy tui` yourself", p.Where)
		} else {
			info("dashboard", "%s (tui.open %q)", p.Where, cmp.Or(mode, "auto"))
		}
	}

	// Claude Code plugin.
	if home, err := os.UserHomeDir(); err == nil {
		pluginDir := filepath.Join(home, setup.ClaudePluginDir)
		installed := false
		if b, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json")); err == nil {
			installed = bytes.Contains(b, []byte(`"tazzyronth@`))
		}
		_, werr := os.Stat(pluginDir)
		switch {
		case installed:
			info("claude plugin", "installed (tazzyronth@...)")
		case werr == nil:
			info("claude plugin", "written to %s but not installed: claude plugin marketplace add %s", pluginDir, pluginDir)
		case !errors.Is(werr, fs.ErrNotExist):
			info("claude plugin", "%v", werr)
		default:
			info("claude plugin", "not installed (run `tazzy init` if you use Claude Code)")
		}
	}

	fmt.Fprintln(w)
	if problems == 0 {
		fmt.Fprintln(w, "No problems found.")
		return 0
	}
	fmt.Fprintf(w, "%d warning(s).\n", problems)
	return 1
}

var gitVersionRE = regexp.MustCompile(`(\d+)\.(\d+)`)

// gitAtLeast reports whether "git version X.Y..." is at least major.minor.
func gitAtLeast(s string, major, minor int) bool {
	m := gitVersionRE.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	x, _ := strconv.Atoi(m[1])
	y, _ := strconv.Atoi(m[2])
	return x > major || x == major && y >= minor
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(filepath.FromSlash(a)), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
