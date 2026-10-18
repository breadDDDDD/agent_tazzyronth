package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tazzyronth/internal/setup"
)

// TestInitPrompts: dry-run writes nothing; each host is confirmed on its own.
func TestInitPrompts(t *testing.T) {
	home := t.TempDir()
	e := setup.Env{
		Home: home, Exe: "/bin/tazzy", Getenv: func(string) string { return "" },
		LookPath: func(n string) (string, error) {
			if n == "gemini" || n == "cursor" {
				return n, nil
			}
			return "", errors.New("no")
		},
		Run: func([]string) error { t.Error("nothing should be run"); return nil },
	}
	var out strings.Builder
	if doInit(e, nil, false, true, strings.NewReader(""), &out); !strings.Contains(out.String(), "settings.json") {
		t.Errorf("dry run output:\n%s", out.String())
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("dry run wrote %v", entries)
	}

	out.Reset()
	if code := doInit(e, nil, false, false, strings.NewReader("y\nn\n"), &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "commands", "tazzy.toml")); err != nil {
		t.Error("gemini (answered y) not set up")
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor", "mcp.json")); err == nil {
		t.Error("cursor (answered n) was set up")
	}

	out.Reset()
	doInit(e, nil, true, false, nil, &out)
	if !strings.Contains(out.String(), "gemini (gemini on PATH)\n  up to date") {
		t.Errorf("second run:\n%s", out.String())
	}
}
