package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"tazzyronth/internal/activity"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/store"
	"tazzyronth/internal/tui"
)

func runTUI(args []string) int {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	dir := fs.String("dir", ".", "project directory")
	noSplash := fs.Bool("no-splash", false, "skip the logo splash")
	frame := fs.String("frame", "", "print one `WxH` frame of the current state and exit (no terminal needed)")
	agent := fs.String("agent", "", "with -frame: render this agent's detail view")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *frame != "" {
		return printFrame(*dir, *frame, *agent)
	}
	enableVT(os.Stdout) // the dashboard needs VT processing even without colour
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := tui.Run(ctx, tui.Options{Dir: *dir, Color: useColor(os.Stdout), Splash: !*noSplash}); err != nil {
		fmt.Fprintln(os.Stderr, "tazzy tui:", err)
		return 1
	}
	return 0
}

func printFrame(dir, size, agent string) int {
	var w, h int
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		fmt.Fprintln(os.Stderr, "tazzy tui: -frame wants WxH, e.g. 120x40")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lines, err := tui.Frame(ctx, dir, w, h, agent, useColor(os.Stdout))
	if err != nil {
		fmt.Fprintln(os.Stderr, "tazzy tui: hub not running:", err)
		hintOthers(dir)
		return 1
	}
	for _, l := range lines {
		fmt.Println(strings.TrimRight(l, " "))
	}
	return 0
}

// runStatus prints a one-shot summary; --line prints one short line for
// host status lines and never fails.
func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	dir := fs.String("dir", ".", "project directory")
	line := fs.Bool("line", false, "one short line (for status lines)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	snap, acts, err := fetchStatus(ctx, *dir, map[bool]int{true: 50, false: 200}[*line])
	if *line {
		if err != nil {
			fmt.Println("tazzy: no hub")
		} else {
			fmt.Println(tui.StatusLine(snap, acts))
		}
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tazzy status: hub not running:", err)
		hintOthers(*dir)
		return 1
	}
	fmt.Print(tui.StatusText(snap, acts, time.Now()))
	return 0
}

// hintOthers names the hubs running for other directories, if any.
func hintOthers(dir string) {
	for _, r := range hub.OtherHubs(dir) {
		fmt.Fprintf(os.Stderr, "  hub running for %s (pid %d): use -dir \"%s\"\n", r.Dir, r.PID, r.Dir)
	}
}

func fetchStatus(ctx context.Context, dir string, limit int) (hub.Snapshot, []activity.Activity, error) {
	var snap hub.Snapshot
	var acts []activity.Activity
	c, err := hub.Connect(dir) // never starts a hub
	if err != nil {
		return snap, nil, err
	}
	defer c.Close()
	if err := c.Call(ctx, "status", nil, &snap); err != nil {
		return snap, nil, err
	}
	err = c.Call(ctx, "activity.list", hub.ActivityListParams{Limit: limit}, &acts)
	return snap, acts, err
}

// runWatch streams hub events to stdout until interrupted or stdout closes,
// reconnecting whenever the hub goes away.
func runWatch(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	dir := fs.String("dir", ".", "project directory")
	asJSON := fs.Bool("json", false, "NDJSON: one event object per line")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out := eventPrinter(os.Stdout, *asJSON)
	if err := watch(ctx, *dir, out); err != nil && ctx.Err() == nil {
		return 1 // stdout closed
	}
	return 0
}

// statusEvery bounds how often watch re-sends the snapshot.
const statusEvery = 500 * time.Millisecond

// watch emits, per connection: a "status" event (data = the snapshot),
// then every hub event as it happens (logged ones with seq > 0, unlogged
// "activity" ones with seq 0), and a fresh "status" at most every
// statusEvery after state changed. Losing the hub emits one "disconnected"
// event; the next connection starts again with "status".
func watch(ctx context.Context, dir string, out func(store.Event) error) error {
	down := false
	for ctx.Err() == nil {
		c, err := hub.Connect(dir)
		if err != nil {
			if !down {
				down = true
				data, _ := json.Marshal(map[string]string{"error": err.Error()})
				if err := out(store.Event{Time: time.Now().UTC(), Type: "disconnected", Data: data}); err != nil {
					return err
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		down = false
		err = watchConn(ctx, c, out)
		c.Close()
		if err != nil {
			return err
		}
		if ctx.Err() == nil {
			down = true
			out(store.Event{Time: time.Now().UTC(), Type: "disconnected", Data: json.RawMessage(`{"error":"hub connection lost"}`)})
		}
	}
	return nil
}

// watchConn streams one connection; it returns an error only if out fails.
func watchConn(ctx context.Context, c *hub.Client, out func(store.Event) error) error {
	events, err := c.SubscribeActivity(ctx)
	if err != nil {
		return nil
	}
	status := func() error {
		var snap json.RawMessage
		if c.Call(ctx, "status", nil, &snap) != nil {
			return nil
		}
		return out(store.Event{Time: time.Now().UTC(), Type: "status", Data: snap})
	}
	if err := status(); err != nil {
		return err
	}
	var due <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if err := out(ev); err != nil {
				return err
			}
			if ev.Seq > 0 && due == nil {
				due = time.After(statusEvery)
			}
		case <-due:
			due = nil
			if err := status(); err != nil {
				return err
			}
		}
	}
}

func eventPrinter(w io.Writer, asJSON bool) func(store.Event) error {
	if asJSON {
		enc := json.NewEncoder(w)
		return func(ev store.Event) error { return enc.Encode(ev) }
	}
	return func(ev store.Event) error {
		ts := ev.Time.Local().Format("15:04:05")
		var err error
		switch ev.Type {
		case "activity":
			var a activity.Activity
			json.Unmarshal(ev.Data, &a)
			_, err = fmt.Fprintf(w, "%s %-8s %-10s %s\n", ts, a.Agent, a.Kind, oneLine(a.Summary))
		case "status":
			var s hub.Snapshot
			json.Unmarshal(ev.Data, &s)
			_, err = fmt.Fprintf(w, "%s status   %s\n", ts, tui.StatusLine(s, nil))
		default:
			_, err = fmt.Fprintf(w, "%s %-19s %s\n", ts, ev.Type, oneLine(string(ev.Data)))
		}
		return err
	}
}

func oneLine(s string) string { return activity.OneLine(s, 160) }
