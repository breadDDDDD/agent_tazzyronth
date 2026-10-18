package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"tazzyronth/internal/setup"
)

func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	yes := fs.Bool("y", false, "apply every change without asking")
	dry := fs.Bool("dry-run", false, "only show what would change")
	only := fs.String("host", "", "comma-separated hosts to set up, detected or not ("+strings.Join(setup.Names(), ", ")+")")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tazzy init:", err)
		return 1
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tazzy init:", err)
		return 1
	}
	e := setup.Env{
		Home: home, Exe: exe, Getenv: os.Getenv, LookPath: exec.LookPath,
		Run: func(argv []string) error {
			c := exec.Command(argv[0], argv[1:]...)
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			return c.Run()
		},
	}
	var names []string
	if *only != "" {
		names = strings.Split(*only, ",")
	}
	return doInit(e, names, *yes, *dry, os.Stdin, os.Stdout)
}

// doInit shows each host's plan, asks (unless yes or dry), and applies it.
func doInit(e setup.Env, only []string, yes, dry bool, in io.Reader, out io.Writer) int {
	hosts := setup.Plan(e, only...)
	if len(hosts) == 0 {
		fmt.Fprintln(out, "No supported agent hosts found ("+strings.Join(setup.Names(), ", ")+"). Use -host to pick one.")
		return 0
	}
	answers := bufio.NewScanner(in)
	status := 0
	for _, h := range hosts {
		fmt.Fprintf(out, "%s (%s)\n", h.Name, h.Why)
		if h.Err != nil {
			fmt.Fprintf(out, "  skipped: %v\n", h.Err)
			status = 1
			continue
		}
		notes := func() {
			for _, n := range h.Notes {
				fmt.Fprintln(out, "  then: "+n)
			}
		}
		if len(h.Changes) == 0 {
			fmt.Fprintln(out, "  up to date")
			notes()
			continue
		}
		for _, c := range h.Changes {
			fmt.Fprintln(out, "  "+c.String())
		}
		if dry {
			notes()
			continue
		}
		if !yes {
			fmt.Fprintf(out, "  apply? [y/N] ")
			ok := answers.Scan() && strings.EqualFold(strings.TrimSpace(answers.Text()), "y")
			if !ok {
				fmt.Fprintln(out, "  skipped")
				continue
			}
		}
		result := "  done"
		for _, c := range h.Changes {
			if err := setup.Apply(e, c); err != nil {
				result, status = "  failed: "+err.Error(), 1
				break
			}
		}
		fmt.Fprintln(out, result)
		notes()
	}
	if !dry {
		fmt.Fprintln(out, "Restart the host, then type /tazzy <goal> (Codex: $tazzy <goal>).")
	}
	return status
}
