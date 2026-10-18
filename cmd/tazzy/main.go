// Command tazzy is the Agent Tazzyronth swarm coordinator.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"tazzyronth/assets"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

type command struct {
	name, summary string
	run           func(args []string) int
}

var commands = []command{
	{"hub", "run the coordinator hub", runHub},
	{"mcp", "run the stdio MCP server (starts the hub if needed)", runMCP},
	{"tui", "open the live dashboard", runTUI},
	{"status", "print a one-shot swarm status (--line: one short line)", runStatus},
	{"watch", "stream swarm events (--json: NDJSON)", runWatch},
	{"init", "install host integrations (MCP server + /tazzy)", runInit},
	{"hook", "entry point for host hooks (hook report)", runHook},
	{"doctor", "check the hub, adapters, git and terminal", runDoctor},
	{"version", "print the version", func([]string) int {
		fmt.Println("tazzy", version)
		return 0
	}},
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		splash(os.Stdout, useColor(os.Stdout))
		return 0
	}
	name := args[0]
	switch name {
	case "-h", "-help", "--help", "help":
		usage(os.Stdout)
		return 0
	case "-v", "-version", "--version":
		name = "version"
	}
	for _, c := range commands {
		if c.name != name {
			continue
		}
		return c.run(args[1:])
	}
	fmt.Fprintf(os.Stderr, "tazzy: unknown command %q\n\n", name)
	usage(os.Stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: tazzy <command> [arguments]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-8s %s\n", c.name, c.summary)
	}
}

func splash(w io.Writer, color bool) {
	logo, title := assets.LogoFullPlain, "AGENT TAZZYRONTH"
	if color {
		logo, title = assets.LogoFull, "\x1b[1;38;2;224;33;138m"+title+"\x1b[0m" // magenta #E0218A
	}
	pad := strings.Repeat(" ", 12) // centres the 16-char title under the 40-col logo
	fmt.Fprint(w, logo)
	fmt.Fprintf(w, "\n%s%s\n%sversion %s\n\n", pad, title, pad, version)
	usage(w)
}

// useColor reports whether f should get truecolor output.
func useColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		// Every console that accepts VT mode (Windows 10+) also does 24-bit colour.
		return enableVT(f)
	}
	ct := os.Getenv("COLORTERM")
	return ct == "truecolor" || ct == "24bit"
}
