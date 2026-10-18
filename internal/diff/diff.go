// Package diff produces unified diffs of leased files as agents edit them
// (plan §3.9 source 3): the Watcher snapshots each file when it is leased,
// polls mtimes, and diffs against the last snapshot on change. A git status
// sweep finds changed files that nobody leased.
package diff

import (
	"fmt"
	"strings"
)

// maxCells bounds the LCS table; beyond it the changed middle is shown as
// one block of removals and additions (still a correct diff, just not minimal).
const maxCells = 4 << 20

type op struct {
	kind byte // ' ', '-', '+'
	line string
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// edits returns a minimal line edit script from a to b (LCS after trimming
// the common prefix and suffix).
func edits(a, b []string) []op {
	var pre, suf int
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	out := make([]op, 0, len(a)+len(b))
	for _, l := range a[:pre] {
		out = append(out, op{' ', l})
	}
	x, y := a[pre:len(a)-suf], b[pre:len(b)-suf]
	n, m := len(x), len(y)
	if n*m > maxCells {
		for _, l := range x {
			out = append(out, op{'-', l})
		}
		for _, l := range y {
			out = append(out, op{'+', l})
		}
	} else {
		// t[i][j] = LCS length of x[i:] and y[j:].
		t := make([]int32, (n+1)*(m+1))
		at := func(i, j int) int32 { return t[i*(m+1)+j] }
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				v := max(at(i+1, j), at(i, j+1))
				if x[i] == y[j] {
					v = at(i+1, j+1) + 1
				}
				t[i*(m+1)+j] = v
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && x[i] == y[j]:
				out = append(out, op{' ', x[i]})
				i, j = i+1, j+1
			case i < n && (j == m || at(i+1, j) >= at(i, j+1)):
				out = append(out, op{'-', x[i]}) // removals first, as diff -u does
				i++
			default:
				out = append(out, op{'+', y[j]})
				j++
			}
		}
	}
	for _, l := range a[len(a)-suf:] {
		out = append(out, op{' ', l})
	}
	return out
}

// Unified returns a unified diff (3 lines of context) from old to new, and
// the number of added and removed lines. It is "" if the line contents are
// equal.
func Unified(path, old, new string) (text string, added, removed int) {
	const ctx = 3
	ops := edits(lines(old), lines(new))
	var changed []int
	for i, o := range ops {
		switch o.kind {
		case '+':
			added++
			changed = append(changed, i)
		case '-':
			removed++
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return "", 0, 0
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	// Old/new line numbers before each op.
	oldAt, newAt := make([]int, len(ops)+1), make([]int, len(ops)+1)
	for i, o := range ops {
		oldAt[i+1], newAt[i+1] = oldAt[i], newAt[i]
		if o.kind != '+' {
			oldAt[i+1]++
		}
		if o.kind != '-' {
			newAt[i+1]++
		}
	}
	for k := 0; k < len(changed); {
		start := max(0, changed[k]-ctx)
		end := changed[k] + 1
		for k++; k < len(changed) && changed[k]-end <= 2*ctx; k++ {
			end = changed[k] + 1
		}
		end = min(len(ops), end+ctx)
		ol, nl := oldAt[end]-oldAt[start], newAt[end]-newAt[start]
		os, ns := oldAt[start]+1, newAt[start]+1
		if ol == 0 {
			os--
		}
		if nl == 0 {
			ns--
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", os, ol, ns, nl)
		for _, o := range ops[start:end] {
			b.WriteByte(o.kind)
			b.WriteString(o.line)
			b.WriteByte('\n')
		}
	}
	return b.String(), added, removed
}
