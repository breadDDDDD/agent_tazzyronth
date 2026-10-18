package launch

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func fakeEnv(goos string, vars map[string]string, onPath ...string) Env {
	return Env{
		Getenv: func(k string) string { return vars[k] },
		GOOS:   goos,
		LookPath: func(name string) (string, error) {
			if slices.Contains(onPath, name) {
				return "/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
	}
}

func TestDecide(t *testing.T) {
	tui := []string{`C:\bin\tazzy.exe`, "tui", "-dir", `C:\my proj`}
	unix := []string{"/usr/bin/tazzy", "tui", "-dir", "/home/me/my proj"}
	all := []string{"tmux", "wezterm", "kitten", "wt", "x-terminal-emulator", "gnome-terminal", "alacritty"}
	cases := []struct {
		name, goos, mode string
		vars             map[string]string
		onPath           []string
		cmd              []string
		where            string
		argv             []string // nil: nothing opens
	}{
		{"wt split", "windows", "", map[string]string{"WT_SESSION": "x"}, all, tui, "Windows Terminal split pane",
			[]string{"wt", "-w", "0", "sp", "-V", "--title", "Tazzyronth", `C:\bin\tazzy.exe`, "tui", "-dir", `C:\my proj`}},
		{"wt in claude code", "windows", "auto", map[string]string{"WT_SESSION": "x", "CLAUDECODE": "1"}, all, tui, "Windows Terminal split pane",
			[]string{"wt", "-w", "0", "sp", "-V", "--title", "Tazzyronth", `C:\bin\tazzy.exe`, "tui", "-dir", `C:\my proj`}},
		{"wt tab", "windows", "tab", map[string]string{"WT_SESSION": "x"}, all, tui, "Windows Terminal tab",
			[]string{"wt", "-w", "0", "nt", "--title", "Tazzyronth", `C:\bin\tazzy.exe`, "tui", "-dir", `C:\my proj`}},
		{"windows console, wt installed", "windows", "", nil, all, tui, "Windows Terminal window",
			[]string{"wt", "-w", "new", "nt", "--title", "Tazzyronth", `C:\bin\tazzy.exe`, "tui", "-dir", `C:\my proj`}},
		{"windows console, no wt", "windows", "", nil, nil, tui, "console window",
			[]string{"cmd", "/c", "start", "", `C:\bin\tazzy.exe`, "tui", "-dir", `C:\my proj`}},
		{"ide terminal ignores inherited WT_SESSION", "windows", "", map[string]string{"WT_SESSION": "x", "TERM_PROGRAM": "vscode"}, all, tui, "Windows Terminal window",
			[]string{"wt", "-w", "new", "nt", "--title", "Tazzyronth", `C:\bin\tazzy.exe`, "tui", "-dir", `C:\my proj`}},
		{"tmux beats wt", "linux", "", map[string]string{"TMUX": "/tmp/t,1,0", "WT_SESSION": "x"}, all, unix, "tmux split pane",
			[]string{"tmux", "split-window", "-h", "/usr/bin/tazzy tui -dir '/home/me/my proj'"}},
		{"tmux without binary falls through", "linux", "", map[string]string{"TMUX": "x", "DISPLAY": ":0"}, []string{"gnome-terminal"}, unix, "GNOME Terminal window",
			[]string{"gnome-terminal", "--", "/usr/bin/tazzy", "tui", "-dir", "/home/me/my proj"}},
		{"wezterm", "darwin", "", map[string]string{"WEZTERM_PANE": "3", "TERM_PROGRAM": "WezTerm"}, all, unix, "WezTerm split pane",
			[]string{"wezterm", "cli", "split-pane", "--right", "--", "/usr/bin/tazzy", "tui", "-dir", "/home/me/my proj"}},
		{"kitty", "linux", "", map[string]string{"KITTY_WINDOW_ID": "1"}, all, unix, "kitty split",
			[]string{"kitten", "@", "launch", "--location=vsplit", "--title", "Tazzyronth", "/usr/bin/tazzy", "tui", "-dir", "/home/me/my proj"}},
		{"macOS Terminal", "darwin", "", map[string]string{"TERM_PROGRAM": "Apple_Terminal"}, nil, unix, "Terminal window",
			[]string{"osascript", "-e", "tell application \"Terminal\"\ndo script \"/usr/bin/tazzy tui -dir '/home/me/my proj'\"\nactivate\nend tell"}},
		{"iTerm2", "darwin", "", map[string]string{"TERM_PROGRAM": "iTerm.app"}, nil, unix, "iTerm2 window",
			[]string{"osascript", "-e", "tell application \"iTerm2\" to create window with default profile command \"/usr/bin/tazzy tui -dir '/home/me/my proj'\""}},
		{"linux $TERMINAL", "linux", "", map[string]string{"DISPLAY": ":0", "TERMINAL": "alacritty"}, all, unix, "alacritty window",
			[]string{"alacritty", "-e", "/usr/bin/tazzy", "tui", "-dir", "/home/me/my proj"}},
		{"linux x-terminal-emulator", "linux", "window", map[string]string{"WAYLAND_DISPLAY": "w"}, all, unix, "terminal window",
			[]string{"x-terminal-emulator", "-e", "/usr/bin/tazzy", "tui", "-dir", "/home/me/my proj"}},
		{"headless linux", "linux", "", nil, all, unix, "no terminal or desktop found", nil},
		{"ssh", "linux", "", map[string]string{"SSH_CONNECTION": "1 2 3 4", "DISPLAY": ":0"}, all, unix, "remote (ssh) session", nil},
		{"tmux over ssh still splits", "linux", "", map[string]string{"SSH_CONNECTION": "x", "TMUX": "x"}, all, unix, "tmux split pane",
			[]string{"tmux", "split-window", "-h", "/usr/bin/tazzy tui -dir '/home/me/my proj'"}},
		{"none", "windows", "none", map[string]string{"WT_SESSION": "x"}, all, tui, "disabled by tui.open", nil},
		{"inline unsupported", "windows", "inline", map[string]string{"WT_SESSION": "x"}, all, tui, "tui.open inline is not supported", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Decide(fakeEnv(c.goos, c.vars, c.onPath...), c.mode, c.cmd)
			if p.Where != c.where || !slices.Equal(p.Argv, c.argv) {
				t.Errorf("got %q %q\nwant %q %q", p.Where, p.Argv, c.where, c.argv)
			}
		})
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"/a/tazzy", "tui", "it's", ""})
	if want := `/a/tazzy tui 'it'\''s' ''`; got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestOpenNothing(t *testing.T) {
	if err := Open(Plan{Where: "nope"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v", err)
	}
}

func TestWeb(t *testing.T) {
	ide := map[string]string{"TERM_PROGRAM": "vscode", "WT_SESSION": "x"}
	for _, c := range []struct {
		mode string
		vars map[string]string
		want bool
	}{
		{"", ide, true}, {"auto", ide, true}, {"web", nil, true},
		{"split", ide, false}, {"", map[string]string{"WT_SESSION": "x"}, false}, {"none", ide, false},
	} {
		if got := WantsWeb(fakeEnv("windows", c.vars), c.mode); got != c.want {
			t.Errorf("WantsWeb(%q, %v) = %v", c.mode, c.vars, got)
		}
	}

	url := "http://127.0.0.1:1234/?token=abc"
	for _, c := range []struct {
		goos   string
		vars   map[string]string
		onPath []string
		argv   []string
	}{
		{"windows", nil, nil, []string{"rundll32", "url.dll,FileProtocolHandler", url}},
		{"darwin", nil, nil, []string{"open", url}},
		{"linux", map[string]string{"DISPLAY": ":0"}, []string{"xdg-open"}, []string{"xdg-open", url}},
		{"linux", map[string]string{"DISPLAY": ":0"}, nil, nil},
		{"linux", nil, []string{"xdg-open"}, nil},
		{"darwin", map[string]string{"SSH_TTY": "/dev/ttys1"}, nil, nil},
	} {
		if p := Browser(fakeEnv(c.goos, c.vars, c.onPath...), url); !slices.Equal(p.Argv, c.argv) {
			t.Errorf("Browser on %s %v: got %q (%s), want %q", c.goos, c.vars, p.Argv, p.Where, c.argv)
		}
	}
}
