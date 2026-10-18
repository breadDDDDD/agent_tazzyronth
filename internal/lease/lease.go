// Package lease tracks file leases: path or glob patterns (src/api/**) held
// by agents. Write leases are exclusive, read leases are shared. A lease on a
// pattern covers everything beneath what it matches, so leasing a directory
// leases its contents.
//
// The table knows nothing about time beyond recording when a lease was taken;
// the hub decides when a lease has expired and releases it explicitly.
package lease

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

type Mode string

const (
	Read  Mode = "read"
	Write Mode = "write"
)

type Lease struct {
	Path   string    `json:"path"`
	Mode   Mode      `json:"mode"`
	Holder string    `json:"holder"`
	TTL    float64   `json:"ttl_s"` // seconds of holder inactivity before it expires
	Since  time.Time `json:"since"`
}

// Conflict is a requested path that overlaps a lease held by someone else.
type Conflict struct {
	Path  string `json:"path"`
	Lease Lease  `json:"held"`
}

func (c Conflict) String() string {
	return fmt.Sprintf("%s overlaps %s (%s-leased by %s)", c.Path, c.Lease.Path, c.Lease.Mode, c.Lease.Holder)
}

// Normalize turns a path or glob into the canonical form leases are stored
// and compared in: forward slashes, cleaned, relative to root, lower-case on
// Windows. "." (the whole project) becomes "**". Backslashes are always
// treated as separators, so glob escapes are not supported.
func Normalize(root, p string) (string, error) {
	orig := p
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	if p == "" {
		return "", errors.New("empty path")
	}
	if runtime.GOOS == "windows" {
		p, root = strings.ToLower(p), strings.ToLower(root)
	}
	if filepath.IsAbs(filepath.FromSlash(p)) || strings.HasPrefix(p, "/") {
		if root == "" {
			return "", fmt.Errorf("%q: absolute path without a project root", orig)
		}
		rel, err := filepath.Rel(root, filepath.FromSlash(p))
		if err != nil {
			return "", fmt.Errorf("%q is outside the project", orig)
		}
		p = filepath.ToSlash(rel)
	}
	p = path.Clean(p)
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("%q is outside the project", orig)
	}
	if p == "." {
		return "**", nil
	}
	for _, seg := range strings.Split(p, "/") {
		if _, err := path.Match(seg, ""); err != nil {
			return "", fmt.Errorf("%q: bad pattern", orig)
		}
	}
	return p, nil
}

// Overlap reports whether two normalized patterns can cover a common path.
// It may answer true when unsure (two globs in the same segment) but never
// answers false for patterns that do overlap.
func Overlap(a, b string) bool {
	// Each pattern covers what it matches and everything beneath it.
	return overlap(append(strings.Split(a, "/"), "**"), append(strings.Split(b, "/"), "**"))
}

func overlap(a, b []string) bool {
	if len(a) > 0 && a[0] == "**" {
		return overlap(a[1:], b) || len(b) > 0 && overlap(a, b[1:])
	}
	if len(b) > 0 && b[0] == "**" {
		return overlap(a, b[1:]) || len(a) > 0 && overlap(a[1:], b)
	}
	if len(a) == 0 || len(b) == 0 {
		return len(a) == 0 && len(b) == 0
	}
	return segOverlap(a[0], b[0]) && overlap(a[1:], b[1:])
}

const wild = "*?["

// segOverlap reports whether two path segments can match the same name.
func segOverlap(x, y string) bool {
	xw, yw := strings.ContainsAny(x, wild), strings.ContainsAny(y, wild)
	switch {
	case !xw && !yw:
		return x == y
	case !xw:
		ok, _ := path.Match(y, x)
		return ok
	case !yw:
		ok, _ := path.Match(x, y)
		return ok
	}
	// Both are globs: any common match starts with both literal prefixes and
	// ends with both literal suffixes, so incompatible ones rule it out.
	xp, yp := x[:strings.IndexAny(x, wild)], y[:strings.IndexAny(y, wild)]
	xs, ys := x[strings.LastIndexAny(x, wild)+1:], y[strings.LastIndexAny(y, wild)+1:]
	return (strings.HasPrefix(xp, yp) || strings.HasPrefix(yp, xp)) &&
		(strings.HasSuffix(xs, ys) || strings.HasSuffix(ys, xs))
}

// Table is the set of live leases. It is not safe for concurrent use.
type Table struct {
	leases []Lease
}

func New() *Table { return &Table{} }

// Conflicts lists the leases held by others that a request by holder for
// paths in mode would collide with. Paths must be normalized.
func (t *Table) Conflicts(holder string, paths []string, mode Mode) []Conflict {
	var out []Conflict
	for _, p := range paths {
		for _, l := range t.leases {
			if l.Holder != holder && (mode == Write || l.Mode == Write) && Overlap(p, l.Path) {
				out = append(out, Conflict{Path: p, Lease: l})
			}
		}
	}
	return out
}

// Acquire grants holder all of paths in mode, or none of them if any
// conflicts (which are returned). Re-acquiring a path the holder already has
// updates its mode and TTL.
func (t *Table) Acquire(holder string, paths []string, mode Mode, ttl float64, now time.Time) []Conflict {
	if c := t.Conflicts(holder, paths, mode); c != nil {
		return c
	}
	for _, p := range paths {
		i := slices.IndexFunc(t.leases, func(l Lease) bool { return l.Holder == holder && l.Path == p })
		if i < 0 {
			t.leases = append(t.leases, Lease{Path: p, Mode: mode, Holder: holder, TTL: ttl, Since: now})
		} else {
			t.leases[i].Mode, t.leases[i].TTL = mode, ttl
		}
	}
	return nil
}

// Release drops holder's leases on paths (exact patterns), or all of them if
// paths is empty, and returns what was released.
func (t *Table) Release(holder string, paths []string) []Lease {
	var out []Lease
	t.leases = slices.DeleteFunc(t.leases, func(l Lease) bool {
		if l.Holder == holder && (len(paths) == 0 || slices.Contains(paths, l.Path)) {
			out = append(out, l)
			return true
		}
		return false
	})
	return out
}

// Held returns holder's leases; List returns everyone's.
func (t *Table) Held(holder string) []Lease {
	out := []Lease{}
	for _, l := range t.leases {
		if l.Holder == holder {
			out = append(out, l)
		}
	}
	return out
}

func (t *Table) List() []Lease { return append([]Lease{}, t.leases...) }
