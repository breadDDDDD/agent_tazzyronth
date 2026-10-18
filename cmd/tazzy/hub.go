package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"tazzyronth/internal/hub"
)

func runHub(args []string) int {
	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	dir := fs.String("dir", ".", "project directory; state lives in <dir>/.tazzyronth")
	webView := fs.Bool("web", false, "also serve the web view on 127.0.0.1 (config \"web\": true does the same)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	h, err := hub.StartWith(*dir, hub.Options{Web: *webView})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tazzy hub:", err)
		return 1
	}
	info := h.Info()
	fmt.Fprintf(os.Stderr, "tazzy hub: listening on %s (pid %d), state in %s\n", info.Addr(), info.PID, hub.StateDir(*dir))
	if info.Web != 0 {
		fmt.Fprintf(os.Stderr, "tazzy hub: web view on http://127.0.0.1:%d/ (token in hub.json)\n", info.Web)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case <-h.Idle():
		fmt.Fprintln(os.Stderr, "tazzy hub: idle (no connections, no workers); exiting")
	}
	if err := h.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "tazzy hub:", err)
		return 1
	}
	return 0
}
