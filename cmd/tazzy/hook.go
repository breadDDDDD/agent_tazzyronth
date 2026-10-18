package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"tazzyronth/internal/hub"
	"tazzyronth/internal/lease"
)

const hookTimeout = 2 * time.Second

func runHook(args []string) int {
	if len(args) == 0 || args[0] != "report" {
		fmt.Fprintln(os.Stderr, "usage: tazzy hook report [-dir DIR] [-agent ID] [-enforce] < hook-payload.json")
		return 2
	}
	return hookReport(args[1:], os.Stdin, os.Stdout)
}

// hookReport forwards a Claude Code hook payload (stdin) to the project's
// hub as activity. It never blocks the host for long and exits 0 whatever
// happens (no hub, bad payload): a hook must not break the host's tool calls.
//
// With -enforce, a PreToolUse for a file-editing tool whose file another
// agent holds a write lease on is denied (a hook decision on stdout).
// Otherwise it prints nothing; with no hub everything is allowed.
func hookReport(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := flag.NewFlagSet("hook report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "project directory (default: $CLAUDE_PROJECT_DIR, else the payload's cwd, else .)")
	agent := fs.String("agent", os.Getenv("TAZZY_AGENT_ID"), "Tazzyronth agent id of the caller, if known")
	enforce := fs.Bool("enforce", false, "deny edits to files another agent holds a write lease on")
	if fs.Parse(args) != nil {
		return 0
	}
	payload, err := io.ReadAll(io.LimitReader(stdin, 16<<20))
	if err != nil || !json.Valid(payload) {
		return 0
	}
	var p struct {
		Cwd   string `json:"cwd"`
		Event string `json:"hook_event_name"`
		Tool  string `json:"tool_name"`
		Input struct {
			File     string `json:"file_path"`
			Notebook string `json:"notebook_path"`
		} `json:"tool_input"`
	}
	json.Unmarshal(payload, &p)
	project := cmp.Or(*dir, os.Getenv("CLAUDE_PROJECT_DIR"), p.Cwd, ".")
	edited := ""
	if *enforce && p.Event == "PreToolUse" && editTools[p.Tool] {
		edited = cmp.Or(p.Input.File, p.Input.Notebook)
	}

	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()
	deny := make(chan string, 1)
	go func() {
		defer close(deny)
		c, err := hub.Connect(project) // never starts a hub
		if err != nil {
			return
		}
		defer c.Close()
		var res hub.HookResult
		if c.Call(ctx, "activity.hook", hub.HookParams{Agent: *agent, Payload: payload}, &res) != nil || edited == "" {
			return
		}
		var check struct {
			Conflicts []lease.Conflict `json:"conflicts"`
		}
		if c.Call(ctx, "lease.check", hub.LeaseCheckParams{Agent: res.Agent, Paths: []string{edited}, Mode: lease.Write}, &check) != nil {
			return
		}
		var held []string
		for _, x := range check.Conflicts {
			if x.Lease.Mode == lease.Write {
				held = append(held, x.Lease.Path+" (write-leased by "+x.Lease.Holder+")")
			}
		}
		if len(held) > 0 {
			deny <- "Tazzyronth: " + edited + " is leased by another agent: " + strings.Join(held, ", ") +
				". Lease it first (tazzy_lease) or ask the holder with tazzy_send."
		}
	}()
	select {
	case reason := <-deny:
		if reason != "" {
			json.NewEncoder(stdout).Encode(map[string]any{"hookSpecificOutput": map[string]string{
				"hookEventName":            "PreToolUse",
				"permissionDecision":       "deny",
				"permissionDecisionReason": reason,
			}})
		}
	case <-ctx.Done():
	}
	return 0
}

// editTools are the Claude Code tools that write a file named in their input.
var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}
