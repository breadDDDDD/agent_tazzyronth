package diff

import (
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/lease"
)

func TestUnifiedExact(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\n"
	new := "a\nb\nc\nD\ne\nf\ng\nh\ni\nj\nk\nl\n"
	got, add, rem := Unified("x.go", old, new)
	want := `--- a/x.go
+++ b/x.go
@@ -1,7 +1,7 @@
 a
 b
 c
-d
+D
 e
 f
 g
@@ -9,3 +9,4 @@
 i
 j
 k
+l
`
	if got != want || add != 2 || rem != 1 {
		t.Errorf("got (+%d -%d)\n%s\nwant\n%s", add, rem, got, want)
	}
	if d, _, _ := Unified("x", old, old); d != "" {
		t.Errorf("equal files diff = %q", d)
	}
	if d, add, _ := Unified("x", "", "one\ntwo\n"); add != 2 || !strings.Contains(d, "@@ -0,0 +1,2 @@") {
		t.Errorf("new file diff = %q", d)
	}
	if d, _, rem := Unified("x", "one\n", ""); rem != 1 || !strings.Contains(d, "@@ -1,1 +0,0 @@") {
		t.Errorf("deleted file diff = %q", d)
	}
}

// apply applies a unified diff produced by Unified to old.
func apply(t *testing.T, old, d string) string {
	t.Helper()
	src := lines(old)
	var out []string
	at := 0 // next unconsumed src line
	for _, l := range strings.Split(strings.TrimSuffix(d, "\n"), "\n") {
		switch {
		case l == "":
		case strings.HasPrefix(l, "---"), strings.HasPrefix(l, "+++"):
		case strings.HasPrefix(l, "@@"):
			f := strings.Fields(l)
			start, _ := strconv.Atoi(strings.Split(f[1][1:], ",")[0])
			if n := strings.Split(f[1], ",")[1]; n == "0" {
				start++ // "-k,0" means "after line k"
			}
			out = append(out, src[at:start-1]...)
			at = start - 1
		case l[0] == ' ' || l[0] == '-':
			if src[at] != l[1:] {
				t.Fatalf("context mismatch at %d: %q vs %q", at, src[at], l[1:])
			}
			if l[0] == ' ' {
				out = append(out, l[1:])
			}
			at++
		case l[0] == '+':
			out = append(out, l[1:])
		}
	}
	out = append(out, src[at:]...)
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

func TestUnifiedRandom(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	gen := func(n int) []string {
		s := make([]string, n)
		for i := range s {
			s[i] = string(rune('a' + r.Intn(4)))
		}
		return s
	}
	join := func(s []string) string {
		if len(s) == 0 {
			return ""
		}
		return strings.Join(s, "\n") + "\n"
	}
	for i := range 500 {
		a := gen(r.Intn(30))
		b := append([]string(nil), a...)
		for range r.Intn(6) { // random edits
			switch p := r.Intn(len(b) + 1); r.Intn(3) {
			case 0:
				b = append(b[:p], append(gen(1+r.Intn(3)), b[p:]...)...)
			case 1:
				if p < len(b) {
					b = append(b[:p], b[p+1:]...)
				}
			case 2:
				if p < len(b) {
					b[p] = "z"
				}
			}
		}
		old, new := join(a), join(b)
		d, add, rem := Unified("f", old, new)
		if got := apply(t, old, d); got != new {
			t.Fatalf("case %d: patch doesn't reproduce new\nold %q\nnew %q\ndiff\n%s", i, old, new, d)
		}
		if add-rem != len(b)-len(a) {
			t.Fatalf("case %d: +%d -%d for %d -> %d lines", i, add, rem, len(a), len(b))
		}
		// Minimal: len(a)+len(b) - 2*LCS changes.
		lcs := 0
		for _, o := range edits(a, b) {
			if o.kind == ' ' {
				lcs++
			}
		}
		if add != len(b)-lcs || rem != len(a)-lcs {
			t.Fatalf("case %d: not minimal", i)
		}
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make sure the mtime moves even on coarse-grained filesystems.
	future := time.Now().Add(time.Duration(len(content)+1) * time.Second)
	os.Chtimes(p, future, future)
}

func norm(t *testing.T, root, p string) string {
	n, err := lease.Normalize(root, p)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWatcher(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.go", "package a\n\nfunc A() {}\n")
	write(t, root, "pkg/b.go", "package b\n")
	write(t, root, "pkg/bin.dat", "\x00\x01\x02")
	w := NewWatcher(root)
	leases := []lease.Lease{
		{Holder: "w1", Path: norm(t, root, "src/a.go"), Mode: lease.Write},
		{Holder: "w2", Path: norm(t, root, "pkg/**"), Mode: lease.Write},
		{Holder: "w3", Path: norm(t, root, "docs/new.md"), Mode: lease.Write}, // doesn't exist yet
		{Holder: "w4", Path: norm(t, root, "README"), Mode: lease.Read},       // read leases aren't watched
	}
	if c := w.Sync(leases); len(c) != 0 {
		t.Fatalf("changes on first sync: %v", c)
	}
	if got := w.Watched(); len(got) != 4 || got["pkg/b.go"] != "w2" {
		t.Fatalf("watched = %v", got)
	}
	if c := w.Poll(); len(c) != 0 {
		t.Fatalf("changes without edits: %v", c)
	}

	write(t, root, "src/a.go", "package a\n\nfunc A() { return }\n")
	write(t, root, "docs/new.md", "# hi\n")
	write(t, root, "pkg/bin.dat", "\x00\x09")
	byPath := map[string]Change{}
	for _, c := range w.Poll() {
		byPath[c.Path] = c
	}
	if c := byPath[norm(t, root, "src/a.go")]; c.Holder != "w1" || c.Added != 1 || c.Removed != 1 || !strings.Contains(c.Diff, "+func A() { return }") {
		t.Errorf("a.go change = %+v", c)
	}
	if c := byPath[norm(t, root, "docs/new.md")]; c.Holder != "w3" || c.Added != 1 {
		t.Errorf("new.md change = %+v", c)
	}
	if c := byPath["pkg/bin.dat"]; c.Holder != "w2" || c.Added != 0 || !strings.Contains(c.Diff, "binary") {
		t.Errorf("binary change = %+v", c)
	}
	if len(byPath) != 3 {
		t.Errorf("changes = %v", byPath)
	}

	// An edit right before the release is still attributed by Sync.
	write(t, root, "pkg/b.go", "package b\n\nvar X = 1\n")
	c := w.Sync(leases[:1])
	if len(c) != 1 || c[0].Holder != "w2" || c[0].Path != "pkg/b.go" || c[0].Added != 2 {
		t.Errorf("final check on release = %+v", c)
	}
	if got := w.Watched(); len(got) != 1 {
		t.Errorf("still watched after release: %v", got)
	}
}

func TestUnattributed(t *testing.T) {
	root := t.TempDir()
	write(t, root, "dirty.txt", "pre-existing\n")
	w := NewWatcher(root)
	leases := []lease.Lease{{Holder: "w1", Path: norm(t, root, "src/**"), Mode: lease.Write}}
	w.Sync(leases)
	if got := w.Unattributed([]string{"dirty.txt"}, leases); len(got) != 0 {
		t.Errorf("baseline sweep alerted: %v", got)
	}
	if got := w.Unattributed([]string{"dirty.txt"}, leases); len(got) != 0 {
		t.Errorf("unchanged file alerted: %v", got)
	}
	write(t, root, "dirty.txt", "changed by someone\n")
	write(t, root, "src/new.go", "package src\n") // under w1's glob, created after the lease
	write(t, root, ".tazzyronth/events.jsonl", "{}\n")
	got := w.Unattributed([]string{"dirty.txt", "src/new.go", ".tazzyronth/events.jsonl"}, leases)
	if len(got) != 1 || got[0] != "dirty.txt" {
		t.Errorf("alerts = %v", got)
	}
	if w.Watched()["src/new.go"] != "w1" {
		t.Errorf("new file under a lease not watched: %v", w.Watched())
	}
}

func TestGitStatus(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	write(t, root, "sub/a.txt", "a\n")
	write(t, root, "b.txt", "b\n")
	write(t, root, ".tazzyronth/x", "x\n")
	write(t, root, ".gitignore", "ignored.txt\n")
	write(t, root, "ignored.txt", "x\n")
	prefix, ok := GitPrefix(filepath.Join(root, "sub"))
	if !ok || prefix != "sub/" {
		t.Fatalf("GitPrefix = %q %v", prefix, ok)
	}
	got, err := GitStatus(root, "")
	if err != nil || strings.Join(got, ",") != ".gitignore,b.txt,sub/a.txt" {
		t.Errorf("GitStatus = %v, %v", got, err)
	}
	if got, _ := GitStatus(filepath.Join(root, "sub"), prefix); strings.Join(got, ",") != "a.txt" {
		t.Errorf("GitStatus(sub) = %v", got)
	}
	if _, ok := GitPrefix(t.TempDir()); ok {
		t.Error("GitPrefix true outside a repo")
	}
}
