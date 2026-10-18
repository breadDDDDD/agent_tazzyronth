package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitAtLeast(t *testing.T) {
	for s, want := range map[string]bool{
		"git version 2.41.0.windows.1": true,
		"git version 2.38.0":           true,
		"git version 2.37.9":           false,
		"git version 3.0":              true,
		"garbage":                      false,
	} {
		if got := gitAtLeast(s, 2, 38); got != want {
			t.Errorf("gitAtLeast(%q) = %v", s, got)
		}
	}
}

func TestDoctorStaleHub(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".tazzyronth"), 0o700)
	// Port 1 on loopback: nothing listens there.
	os.WriteFile(filepath.Join(dir, ".tazzyronth", "hub.json"), []byte(`{"port":1,"token":"x","pid":1}`), 0o600)
	var buf bytes.Buffer
	if doctor(&buf, dir) == 0 {
		t.Error("stale hub.json should be a warning")
	}
	out := buf.String()
	for _, want := range []string{"stale hub.json", "adapter claude", "isolation", "dashboard"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
