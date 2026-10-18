package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tazzyronth/internal/config"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/launch"
)

// TestDashboardPlan checks what tazzy_start would open; nothing is opened.
func TestDashboardPlan(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	h, err := hub.StartWith(dir, hub.Options{Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	env := func(vars map[string]string) launch.Env {
		return launch.Env{
			Getenv:   func(k string) string { return vars[k] },
			GOOS:     "windows",
			LookPath: func(string) (string, error) { return "", errors.New("no") },
		}
	}

	// IDE terminal: auto → the web view in the browser, web server started on demand.
	p, url, err := dashboardPlan(dir, env(map[string]string{"TERM_PROGRAM": "vscode"}))
	if err != nil {
		t.Fatal(err)
	}
	info, _ := hub.ReadInfo(dir)
	if info.Web == 0 || !strings.Contains(url, "token="+info.Token) {
		t.Fatalf("url %q, hub.json %+v", url, info)
	}
	if want := []string{"rundll32", "url.dll,FileProtocolHandler", url}; !slices.Equal(p.Argv, want) {
		t.Errorf("argv %q, want %q", p.Argv, want)
	}

	// A plain console: the TUI in a window.
	p, url, err = dashboardPlan(dir, env(nil))
	if err != nil || url != "" || len(p.Argv) < 3 || p.Argv[0] != "cmd" || !slices.Contains(p.Argv, "tui") {
		t.Errorf("console: %q %q %v", p.Argv, url, err)
	}

	// tui.open "web" in the config wins anywhere.
	os.WriteFile(filepath.Join(hub.StateDir(dir), "config.json"), []byte(`{"tui":{"open":"web"}}`), 0o600)
	if p, url, err = dashboardPlan(dir, env(nil)); err != nil || url == "" || p.Argv[0] != "rundll32" {
		t.Errorf("tui.open web: %q %q %v", p.Argv, url, err)
	}
}
