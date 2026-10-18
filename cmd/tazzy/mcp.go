package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"tazzyronth/internal/config"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/launch"
	"tazzyronth/internal/mcp"
)

const (
	hubStartTimeout = 10 * time.Second
	staleLock       = 30 * time.Second
)

func runMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	dir := fs.String("dir", ".", "project directory; the hub's state lives in <dir>/.tazzyronth")
	agent := fs.String("agent", "", "agent id the hub pre-registered for this worker (set in hub-spawned workers' MCP configs)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tazzy mcp:", err)
		return 1
	}
	// stdout carries the protocol; everything else goes to stderr.
	logger := log.New(os.Stderr, "tazzy mcp: ", log.LstdFlags)
	connect := func(ctx context.Context) (*hub.Client, error) { return connectOrStart(ctx, abs, logger) }
	srv := mcp.New(connect, version, logger)
	if *agent != "" {
		srv.SetAgent(*agent)
	} else {
		srv.SetLauncher(func() string { return openDashboard(abs, launch.System()) })
	}
	if err := srv.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		logger.Print(err)
		return 1
	}
	return 0
}

// connectOrStart connects to the project's hub, starting a detached
// "tazzy hub" first if none is running. A lock file keeps concurrently
// starting shims from launching two hubs.
func connectOrStart(ctx context.Context, dir string, logger *log.Logger) (*hub.Client, error) {
	if c, err := hub.Connect(dir); err == nil {
		return c, nil
	}
	if err := os.MkdirAll(hub.StateDir(dir), 0o700); err != nil {
		return nil, err
	}
	lock := filepath.Join(hub.StateDir(dir), "hub.lock")
	if acquireLock(lock) {
		defer os.Remove(lock)
		if err := spawnHub(dir); err != nil {
			return nil, fmt.Errorf("start hub: %w", err)
		}
		logger.Printf("started hub for %s", dir)
	}

	deadline := time.Now().Add(hubStartTimeout)
	for {
		c, err := hub.Connect(dir)
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("hub did not come up within %s (see %s): %w",
				hubStartTimeout, filepath.Join(hub.StateDir(dir), "hub.log"), err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func acquireLock(path string) bool {
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return true
		}
		if !errors.Is(err, os.ErrExist) {
			return false
		}
		// Left behind by a shim that died mid-start?
		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) > staleLock {
			os.Remove(path)
			continue
		}
		return false
	}
	return false
}

func spawnHub(dir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(hub.StateDir(dir), "hub.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "hub", "-dir", dir)
	// Explicit files so the hub never inherits the host's stdio pipes.
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = detached()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// openDashboard opens the dashboard for the project (config tui.open): tazzy
// tui next to the user's terminal, or the web view in the browser. It says
// how it went, for the tazzy_start result.
func openDashboard(dir string, env launch.Env) string {
	p, url, err := dashboardPlan(dir, env)
	if err == nil {
		err = launch.Open(p)
	}
	switch {
	case err == nil && url != "":
		return "opened the web view in the " + p.Where + ": " + url
	case err == nil:
		return "opened in a " + p.Where
	case url != "":
		return "not opened (" + err.Error() + "): ask the user to open " + url
	}
	return "not opened (" + err.Error() + "): ask the user to run `tazzy tui -dir " + dir + "` in another terminal"
}

// dashboardPlan decides what openDashboard runs. For the web view it starts
// the hub's web server (web.start) and returns its URL too.
func dashboardPlan(dir string, env launch.Env) (launch.Plan, string, error) {
	cfg, err := config.Load(dir)
	if err != nil {
		return launch.Plan{}, "", err
	}
	if launch.WantsWeb(env, cfg.TUI.Open) {
		c, err := hub.Connect(dir)
		if err != nil {
			return launch.Plan{}, "", err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var res hub.WebResult
		if err := c.Call(ctx, "web.start", nil, &res); err != nil {
			return launch.Plan{}, "", err
		}
		return launch.Browser(env, res.URL), res.URL, nil
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "tazzy"
	}
	return launch.Decide(env, cfg.TUI.Open, []string{exe, "tui", "-dir", dir}), "", nil
}
