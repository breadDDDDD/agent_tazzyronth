package diff

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"tazzyronth/internal/lease"
)

const (
	MaxFileSize   = 1 << 20 // larger files are not diffed
	maxPerPattern = 200     // files snapshotted per glob/directory lease
)

// Change is an edit to a watched file.
type Change struct {
	Holder, Path   string // lease holder; path relative to the root, slash-separated
	Diff           string
	Added, Removed int
}

type stamp struct {
	mod  time.Time
	size int64 // -1: missing
}

type file struct {
	path    string // display path
	holder  string
	pattern string // the lease that brought it in
	dir     string // where the holder works: the root, or its worktree
	content string
	skip    bool // binary or too large: changes are noticed but not diffed
	stamp
}

// Watcher tracks the files under write leases. It is not safe for
// concurrent use.
type Watcher struct {
	root string
	// Dir, if set, is where a lease holder's files are (its worktree in
	// worktree isolation); "" means the root.
	Dir      func(holder string) string
	files    map[string]*file  // key: lease.Normalize'd path
	patterns map[string]string // write lease path -> holder, as of the last Sync
	known    map[string]stamp  // last stamp seen of any file, watched or swept
	swept    bool
}

func NewWatcher(root string) *Watcher {
	return &Watcher{root: root, files: map[string]*file{}, patterns: map[string]string{}, known: map[string]stamp{}}
}

// Sync makes the watched set follow the write leases: files whose lease is
// gone are checked one last time (so an edit just before the release is
// still attributed) and dropped; newly leased files are snapshotted.
func (w *Watcher) Sync(leases []lease.Lease) []Change {
	cur := map[string]string{}
	for _, l := range leases {
		if l.Mode == lease.Write {
			cur[l.Path] = l.Holder
		}
	}
	var out []Change
	for k, f := range w.files {
		if cur[f.pattern] != f.holder {
			if c, ok := w.check(k, f); ok {
				out = append(out, c)
			}
			delete(w.files, k)
		}
	}
	for p, holder := range cur {
		if w.patterns[p] != holder {
			w.expand(p, holder)
		}
	}
	w.patterns = cur
	return out
}

// Poll returns the changes to watched files since the last look.
func (w *Watcher) Poll() []Change {
	var out []Change
	for k, f := range w.files {
		if c, ok := w.check(k, f); ok {
			out = append(out, c)
		}
	}
	return out
}

// Watched reports the files being watched and their holders (for tests).
func (w *Watcher) Watched() map[string]string {
	m := map[string]string{}
	for _, f := range w.files {
		m[f.path] = f.holder
	}
	return m
}

func hasMeta(s string) bool { return strings.ContainsAny(s, "*?[") }

// expand snapshots the files a lease pattern covers: an exact path (which
// may not exist yet: it is then diffed against empty once created), or the
// existing files under a directory or glob, up to maxPerPattern.
func (w *Watcher) expand(pattern, holder string) {
	dir := w.dirOf(holder)
	abs := filepath.Join(dir, filepath.FromSlash(pattern))
	if !hasMeta(pattern) {
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			w.add(pattern, pattern, pattern, holder)
			return
		}
	}
	base := pattern
	for hasMeta(base) {
		base = path.Dir(base)
	}
	n := 0
	filepath.WalkDir(filepath.Join(dir, filepath.FromSlash(base)), func(p string, d fs.DirEntry, err error) error {
		if n >= maxPerPattern {
			return filepath.SkipAll
		}
		if err != nil {
			return filepath.SkipDir
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".tazzyronth" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		disp := filepath.ToSlash(rel)
		if key, err := lease.Normalize(dir, disp); err == nil && d.Type().IsRegular() && lease.Overlap(key, pattern) {
			w.add(key, disp, pattern, holder)
			n++
		}
		return nil
	})
}

func (w *Watcher) add(key, disp, pattern, holder string) {
	if f := w.files[key]; f != nil {
		f.holder, f.pattern = holder, pattern // already watched: keep the snapshot
		return
	}
	f := &file{path: disp, holder: holder, pattern: pattern, dir: w.dirOf(holder)}
	f.stamp, f.content, f.skip = read(f.dir, key)
	w.files[key] = f
	w.known[key] = f.stamp
}

func (w *Watcher) dirOf(holder string) string {
	if w.Dir != nil {
		if d := w.Dir(holder); d != "" {
			return d
		}
	}
	return w.root
}

func stat(dir, key string) stamp {
	fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key)))
	if err != nil || fi.IsDir() {
		return stamp{size: -1}
	}
	return stamp{mod: fi.ModTime(), size: fi.Size()}
}

// read returns a file's stamp and text; skip is set for binary or huge files.
func read(dir, key string) (st stamp, text string, skip bool) {
	st = stat(dir, key)
	if st.size < 0 {
		return st, "", false
	}
	if st.size > MaxFileSize {
		return st, "", true
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
	if err != nil {
		return stamp{size: -1}, "", false
	}
	if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
		return st, "", true
	}
	return st, string(b), false
}

func (w *Watcher) check(key string, f *file) (Change, bool) {
	if stat(f.dir, key) == f.stamp {
		return Change{}, false
	}
	st, text, skip := read(f.dir, key)
	old, wasSkipped := f.content, f.skip
	f.stamp, f.content, f.skip = st, text, skip
	w.known[key] = st
	if skip || wasSkipped {
		return Change{Holder: f.holder, Path: f.path, Diff: "(binary or large file changed)"}, true
	}
	d, add, rem := Unified(f.path, old, text)
	if d == "" {
		return Change{}, false
	}
	return Change{Holder: f.holder, Path: f.path, Diff: d, Added: add, Removed: rem}, true
}

// Unattributed takes the paths git reports as changed (relative,
// slash-separated) and returns those that changed since the previous sweep
// while no write lease covered them. The first sweep only records a
// baseline. Changed files under a lease the watcher missed (new files under
// a glob) start being watched.
func (w *Watcher) Unattributed(paths []string, leases []lease.Lease) []string {
	first := !w.swept
	w.swept = true
	var out []string
	for _, p := range paths {
		key, err := lease.Normalize(w.root, p)
		if err != nil || key == ".tazzyronth" || strings.HasPrefix(key, ".tazzyronth/") {
			continue
		}
		st := stat(w.root, key)
		if prev, ok := w.known[key]; ok && prev == st {
			continue
		}
		w.known[key] = st
		if first || w.files[key] != nil {
			continue
		}
		holder, pattern := "", ""
		for _, l := range leases {
			if l.Mode == lease.Write && lease.Overlap(key, l.Path) {
				holder, pattern = l.Holder, l.Path
				break
			}
		}
		if holder != "" {
			w.add(key, p, pattern, holder)
			continue
		}
		out = append(out, p)
	}
	return out
}

// GitPrefix reports whether root is inside a git work tree (and git is
// installed), and root's path within it ("" at the top, else "dir/").
func GitPrefix(root string) (string, bool) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree", "--show-prefix").Output()
	f := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err != nil || f[0] != "true" {
		return "", false
	}
	if len(f) > 1 {
		return strings.TrimSpace(f[1]), true
	}
	return "", true
}

// GitStatus lists the changed and untracked files under root (relative to
// it, slash-separated), honouring .gitignore and skipping .tazzyronth.
// prefix is GitPrefix's: porcelain paths are relative to the repo top.
func GitStatus(root, prefix string) ([]string, error) {
	cmd := exec.Command("git", "-c", "core.quotepath=off", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", ".", ":(exclude).tazzyronth")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var paths []string
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		e := fields[i]
		if len(e) < 4 {
			continue
		}
		if p, ok := strings.CutPrefix(e[3:], prefix); ok {
			paths = append(paths, p)
		}
		if e[0] == 'R' || e[0] == 'C' {
			i++ // the original path follows a rename/copy
		}
	}
	return paths, nil
}
