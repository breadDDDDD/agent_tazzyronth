// Package launch opens the dashboard next to the agent CLI the user is in
// (plan §7.2): it works out the terminal from the environment and builds the
// command that opens tazzy tui in a split pane, tab or window there, or the
// web view in a browser.
package launch

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Placements (config "tui.open"). "" and "auto" pick per terminal.
const (
	Auto   = "auto"
	Split  = "split"
	Tab    = "tab"
	Window = "window"
	Web    = "web" // the web view in the default browser
	None   = "none"
)

// Env is what Decide looks at; System() is the real one.
type Env struct {
	Getenv   func(string) string
	GOOS     string
	LookPath func(string) (string, error)
}

func System() Env {
	return Env{Getenv: os.Getenv, GOOS: runtime.GOOS, LookPath: exec.LookPath}
}

// Plan is what to run. Argv is nil when nothing can be opened; Where then
// says why.
type Plan struct {
	Where string   // e.g. "Windows Terminal split pane"
	Argv  []string // command that opens it (returns quickly or is left running)
}

// Decide picks where to open cmd (the tui command line, absolute exe first)
// for the placement mode. Claude Code (CLAUDECODE=1) gets the same as its
// terminal for now; its in-terminal pane is the plugin mod's job.
func Decide(e Env, mode string, cmd []string) Plan {
	switch mode {
	case "", Auto:
		mode = Split // every terminal falls back to what it can do
	case Split, Tab, Window:
	case None:
		return Plan{Where: "disabled by tui.open"}
	default: // inline is not implemented; web goes through WantsWeb + Browser
		return Plan{Where: "tui.open " + mode + " is not supported"}
	}
	has := func(name string) bool { _, err := e.LookPath(name); return err == nil }
	env := e.Getenv

	// Multiplexers first: inside tmux in Windows Terminal, a tmux split is
	// the one next to the user.
	switch {
	case env("TMUX") != "" && has("tmux"):
		if mode == Split {
			return Plan{"tmux split pane", []string{"tmux", "split-window", "-h", shellJoin(cmd)}}
		}
		if mode == Tab {
			return Plan{"tmux window", []string{"tmux", "new-window", "-n", "tazzyronth", shellJoin(cmd)}}
		}
	case env("TERM_PROGRAM") == "vscode":
		// An IDE terminal (VS Code, Cursor, Antigravity) may have inherited
		// WT_SESSION and the like, but a pane there would not be next to it.
		return window(e, cmd, has)
	case env("WEZTERM_PANE") != "" && has("wezterm"):
		if mode == Split {
			return Plan{"WezTerm split pane", append([]string{"wezterm", "cli", "split-pane", "--right", "--"}, cmd...)}
		}
		if mode == Tab {
			return Plan{"WezTerm tab", append([]string{"wezterm", "cli", "spawn", "--"}, cmd...)}
		}
	case env("KITTY_WINDOW_ID") != "" && has("kitten"):
		// Needs allow_remote_control in kitty.conf; failure is reported.
		if mode == Split {
			return Plan{"kitty split", append([]string{"kitten", "@", "launch", "--location=vsplit", "--title", "Tazzyronth"}, cmd...)}
		}
		if mode == Tab {
			return Plan{"kitty tab", append([]string{"kitten", "@", "launch", "--type=tab", "--tab-title", "Tazzyronth"}, cmd...)}
		}
	case env("WT_SESSION") != "" && has("wt"):
		sub := map[string][]string{
			Split:  {"-w", "0", "sp", "-V"},
			Tab:    {"-w", "0", "nt"},
			Window: {"-w", "new", "nt"},
		}[mode]
		where := map[string]string{Split: "Windows Terminal split pane", Tab: "Windows Terminal tab", Window: "Windows Terminal window"}[mode]
		return Plan{where, append(append(append([]string{"wt"}, sub...), "--title", "Tazzyronth"), cmd...)}
	}
	return window(e, cmd, has)
}

// window opens a new OS terminal window.
func window(e Env, cmd []string, has func(string) bool) Plan {
	env := e.Getenv
	if env("SSH_CONNECTION") != "" || env("SSH_TTY") != "" {
		return Plan{Where: "remote (ssh) session"} // a window would open on the far machine
	}
	switch e.GOOS {
	case "windows":
		if has("wt") {
			return Plan{"Windows Terminal window", append([]string{"wt", "-w", "new", "nt", "--title", "Tazzyronth"}, cmd...)}
		}
		// start takes its first quoted argument as the window title.
		return Plan{"console window", append([]string{"cmd", "/c", "start", ""}, cmd...)}
	case "darwin":
		if env("TERM_PROGRAM") == "iTerm.app" {
			script := "tell application \"iTerm2\" to create window with default profile command " + appleQuote(shellJoin(cmd))
			return Plan{"iTerm2 window", []string{"osascript", "-e", script}}
		}
		script := "tell application \"Terminal\"\ndo script " + appleQuote(shellJoin(cmd)) + "\nactivate\nend tell"
		return Plan{"Terminal window", []string{"osascript", "-e", script}}
	}
	if env("DISPLAY") == "" && env("WAYLAND_DISPLAY") == "" {
		return Plan{Where: "no terminal or desktop found"}
	}
	if t := env("TERMINAL"); t != "" && has(t) {
		return Plan{t + " window", append([]string{t, "-e"}, cmd...)}
	}
	if has("x-terminal-emulator") {
		return Plan{"terminal window", append([]string{"x-terminal-emulator", "-e"}, cmd...)}
	}
	if has("gnome-terminal") {
		return Plan{"GNOME Terminal window", append([]string{"gnome-terminal", "--"}, cmd...)}
	}
	return Plan{Where: "no terminal emulator found"}
}

// Open starts p without waiting for it.
func Open(p Plan) error {
	if p.Argv == nil {
		return errors.New(p.Where)
	}
	c := exec.Command(p.Argv[0], p.Argv[1:]...)
	if err := c.Start(); err != nil {
		return err
	}
	go c.Wait() // reap; most of these exit as soon as the pane is open
	return nil
}

// shellJoin quotes argv for a POSIX shell.
func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@") == "" {
			q[i] = a
		} else {
			q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(q, " ")
}

// appleQuote makes an AppleScript string literal.
func appleQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// WantsWeb reports whether mode means the web view: "web", or auto in an IDE
// integrated terminal (VS Code, Cursor, Antigravity set TERM_PROGRAM=vscode),
// where a browser tab is closer than a new OS terminal window.
func WantsWeb(e Env, mode string) bool {
	return mode == Web || (mode == "" || mode == Auto) && e.Getenv("TERM_PROGRAM") == "vscode"
}

// Browser opens url in the default browser.
func Browser(e Env, url string) Plan {
	env := e.Getenv
	if env("SSH_CONNECTION") != "" || env("SSH_TTY") != "" {
		return Plan{Where: "remote (ssh) session"}
	}
	switch e.GOOS {
	case "windows":
		// Not "cmd /c start": cmd would treat & in the URL as a separator.
		return Plan{"browser", []string{"rundll32", "url.dll,FileProtocolHandler", url}}
	case "darwin":
		return Plan{"browser", []string{"open", url}}
	}
	if env("DISPLAY") == "" && env("WAYLAND_DISPLAY") == "" {
		return Plan{Where: "no desktop found"}
	}
	if _, err := e.LookPath("xdg-open"); err != nil {
		return Plan{Where: "xdg-open not found"}
	}
	return Plan{"browser", []string{"xdg-open", url}}
}
