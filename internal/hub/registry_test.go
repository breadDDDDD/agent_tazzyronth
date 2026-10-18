package hub

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistry(t *testing.T) {
	reg := t.TempDir()
	t.Setenv(RegistryEnv, reg)
	a, b := t.TempDir(), t.TempDir()
	ha, err := Start(a)
	if err != nil {
		t.Fatal(err)
	}
	defer ha.Close()
	hb, err := Start(b)
	if err != nil {
		t.Fatal(err)
	}

	if got := RunningHubs(); len(got) != 2 {
		t.Fatalf("RunningHubs = %+v, want 2", got)
	}
	others := OtherHubs(a)
	if len(others) != 1 || others[0].Dir != b || others[0].PID != os.Getpid() {
		t.Fatalf("OtherHubs(a) = %+v, want only %s", others, b)
	}

	hb.Close()
	if got := OtherHubs(a); len(got) != 0 {
		t.Fatalf("after close OtherHubs(a) = %+v", got)
	}

	// A crashed hub leaves its entry and hub.json behind; listing prunes it.
	register(Running{Dir: b, PID: 1, Port: 1})
	writeInfo(b, Info{Port: 1, PID: 1, Token: "x"})
	if got := OtherHubs(a); len(got) != 0 {
		t.Fatalf("stale entry listed: %+v", got)
	}
	if files, _ := filepath.Glob(filepath.Join(reg, "*.json")); len(files) != 1 {
		t.Fatalf("registry files = %v, want only a's", files)
	}
}
