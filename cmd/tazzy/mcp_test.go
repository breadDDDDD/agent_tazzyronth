package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tazzyronth/internal/config"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/launch"
	"tazzyronth/internal/mcp"
	"tazzyronth/internal/proto"
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

// shimCall runs one tazzy_start through a shim built by newShim, as a client
// without the roots capability would, and returns the tool result.
func shimCall(t *testing.T, srv *mcp.Server) (text string, isErr bool) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), inR, outW); outW.Close() }()
	defer func() {
		inW.Close()
		go io.Copy(io.Discard, outR)
		<-done
	}()
	w, r := proto.NewWriter(inW), proto.NewReader(outR)
	send := func(id int, method string, params any) *proto.Message {
		m, _ := proto.NewRequest(id, method, params)
		if err := w.Write(m); err != nil {
			t.Fatal(err)
		}
		resp, err := r.Read()
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	send(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	n, _ := proto.NewNotification("notifications/initialized", nil)
	w.Write(n)
	resp := send(2, "tools/call", map[string]any{"name": "tazzy_start", "arguments": map[string]any{"goal": "g", "spawn": "host"}})
	if resp.Error != nil {
		t.Fatalf("tools/call: protocol error %+v, want a tool result", resp.Error)
	}
	var res struct {
		IsError bool `json:"isError"`
		Content []struct{ Text string }
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil || len(res.Content) == 0 {
		t.Fatalf("result %s: %v", resp.Result, err)
	}
	return res.Content[0].Text, res.IsError
}

// TestShimProjectGuard: a cwd fallback (no -dir, no $TAZZY_PROJECT, no
// client root) is used only if it is a project.
func TestShimProjectGuard(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	// inProcess starts dir's hub in this process instead of a detached one.
	inProcess := func(t *testing.T, started *[]string) func(context.Context, string) (*hub.Client, error) {
		return func(ctx context.Context, dir string) (*hub.Client, error) {
			*started = append(*started, dir)
			cfg := config.Default()
			h, err := hub.StartWith(dir, hub.Options{Config: &cfg})
			if err != nil {
				return nil, err
			}
			t.Cleanup(func() { h.Close() })
			return hub.Connect(dir)
		}
	}
	noOpen := func(string) string { return "not opened (test)" }
	plain := func(t *testing.T) string {
		d := t.TempDir()
		if isProject(d) {
			t.Skipf("%s is inside a git work tree; cannot test a non-project dir", d)
		}
		return d
	}

	t.Run("plain cwd refused", func(t *testing.T) {
		dir := plain(t)
		var started []string
		opened := false
		srv := newShim(dir, false, "", logger, inProcess(t, &started), func(string) string { opened = true; return "" })
		text, isErr := shimCall(t, srv)
		if !isErr || !strings.HasPrefix(text, "no project folder:") || !strings.Contains(text, dir) || !strings.Contains(text, `"-dir"`) {
			t.Errorf("tazzy_start = %q (error %v), want the no-project error naming %s", text, isErr, dir)
		}
		if len(started) > 0 || opened {
			t.Errorf("hub started for %v, dashboard opened %v", started, opened)
		}
		if _, err := os.Stat(hub.StateDir(dir)); !os.IsNotExist(err) {
			t.Errorf(".tazzyronth created in the refused dir: %v", err)
		}
	})

	t.Run("cwd with .tazzyronth accepted", func(t *testing.T) {
		dir := plain(t)
		os.MkdirAll(hub.StateDir(dir), 0o700)
		var started []string
		text, isErr := shimCall(t, newShim(dir, false, "", logger, inProcess(t, &started), noOpen))
		if isErr || len(started) != 1 || started[0] != dir {
			t.Errorf("tazzy_start = %q (error %v), started %v", text, isErr, started)
		}
		if _, err := hub.ReadInfo(dir); err != nil {
			t.Errorf("no hub.json: %v", err)
		}
	})

	t.Run("cwd in a git work tree accepted", func(t *testing.T) {
		dir := plain(t)
		os.Mkdir(filepath.Join(dir, ".git"), 0o700)
		sub := filepath.Join(dir, "sub")
		os.Mkdir(sub, 0o700)
		if !isProject(sub) {
			t.Fatal("isProject false below a .git dir")
		}
	})

	t.Run("explicit plain dir accepted", func(t *testing.T) {
		dir := plain(t)
		var started []string
		text, isErr := shimCall(t, newShim(dir, true, "", logger, inProcess(t, &started), noOpen))
		if isErr || len(started) != 1 {
			t.Errorf("tazzy_start = %q (error %v), started %v", text, isErr, started)
		}
		if _, err := hub.ReadInfo(dir); err != nil {
			t.Errorf("no hub.json: %v", err)
		}
	})
}
