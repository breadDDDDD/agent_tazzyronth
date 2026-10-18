package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSplash(t *testing.T) {
	for _, color := range []bool{false, true} {
		var buf bytes.Buffer
		splash(&buf, color)
		out := buf.String()
		if got := strings.Contains(out, "\x1b["); got != color {
			t.Errorf("color=%v: contains escapes = %v", color, got)
		}
		for _, want := range []string{"AGENT TAZZYRONTH", version, "Usage: tazzy"} {
			if !strings.Contains(out, want) {
				t.Errorf("color=%v: missing %q", color, want)
			}
		}
	}
}

func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		args []string
		want int
	}{
		{[]string{"version"}, 0},
		{[]string{"--version"}, 0},
		{[]string{"tui"}, 1},
		{[]string{"hub", "-bogus"}, 2},
		{[]string{"nope"}, 2},
	}
	for _, tt := range tests {
		if got := run(tt.args); got != tt.want {
			t.Errorf("run(%v) = %d, want %d", tt.args, got, tt.want)
		}
	}
}

func TestUseColorRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if useColor(nil) {
		t.Error("useColor = true with NO_COLOR set")
	}
}
