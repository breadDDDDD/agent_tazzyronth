package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenNeedsRepoTopWithCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	plain := t.TempDir()
	if _, err := Open(plain); err == nil || !strings.Contains(err.Error(), "needs a git repository") {
		t.Errorf("not a repo: %v", err)
	}
	dir := t.TempDir()
	run(dir, "init", "-q")
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "at least one commit") {
		t.Errorf("no commit: %v", err)
	}
	run(dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init")
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	if _, err := Open(sub); err == nil || !strings.Contains(err.Error(), "top level") {
		t.Errorf("subdir: %v", err)
	}
	if _, err := Open(dir); err != nil {
		t.Error(err)
	}
	if b := Branch("a b/../c"); b != "tazzy/a-b----c" {
		t.Errorf("Branch = %q", b)
	}
}
