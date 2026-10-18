package lease

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	root, _ := filepath.Abs("proj")
	tests := []struct{ in, want string }{
		{"src/api/x.go", "src/api/x.go"},
		{`src\api\x.go`, "src/api/x.go"},
		{"./src//api/../api/", "src/api"},
		{"src/api/**", "src/api/**"},
		{".", "**"},
		{filepath.Join(root, "src", "a.go"), "src/a.go"},
		{"../other", ""},
		{filepath.Join(root, "..", "x"), ""},
		{"", ""},
		{"src/[a", ""},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests, struct{ in, want string }{`SRC\Api\X.go`, "src/api/x.go"})
	}
	for _, tt := range tests {
		got, err := Normalize(root, tt.in)
		if tt.want == "" {
			if err == nil {
				t.Errorf("Normalize(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
		if again, _ := Normalize(root, got); again != got {
			t.Errorf("Normalize not idempotent: %q -> %q", got, again)
		}
	}
}

func TestOverlap(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"src/a.go", "src/a.go", true},
		{"src/a.go", "src/b.go", false},
		{"src", "src/a.go", true}, // a directory covers its contents
		{"src/api/**", "src/api/v1/h.go", true},
		{"src/api/**", "src/api", true},
		{"src/api/**", "src/apix/h.go", false},
		{"src/*.go", "src/a.go", true},
		{"src/*.go", "src/a.ts", false},
		{"src/*.go", "src/sub/a.ts", false},
		{"**", "anything/at/all", true},
		{"**/*.go", "src/deep/a.go", true},
		{"**/*.go", "src/deep/a.ts", true}, // conservative: a.ts might be a directory
		{"src/**/*.go", "web/a.go", false},
		{"src/*/h.go", "src/api/**", true},
		{"src/a*", "src/b*", false},
		{"src/*.go", "src/*_test.go", true},
		{"src/*.go", "src/*.ts", false},
		{"web/**", "src/**", false},
		{"a/**/z.go", "a/b/c/z.go", true},
		{"a/**/z.go", "a/b/c/y.go", true}, // likewise: y.go/z.go
		{"a/*/z.go", "a/b/c/y.go", false},
	}
	for _, tt := range tests {
		if got := Overlap(tt.a, tt.b); got != tt.want {
			t.Errorf("Overlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
		if got := Overlap(tt.b, tt.a); got != tt.want {
			t.Errorf("Overlap(%q, %q) = %v, want %v (reversed)", tt.b, tt.a, got, tt.want)
		}
	}
}

func TestTable(t *testing.T) {
	tb := New()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if c := tb.Acquire("a", []string{"src/api/**"}, Write, 60, now); c != nil {
		t.Fatalf("first acquire: %v", c)
	}
	// Writes and reads overlapping someone else's write both conflict, naming the holder.
	c := tb.Acquire("b", []string{"docs/x.md", "src/api/h.go"}, Write, 60, now)
	if len(c) != 1 || c[0].Lease.Holder != "a" || c[0].Path != "src/api/h.go" {
		t.Fatalf("conflict = %v", c)
	}
	if c := tb.Acquire("b", []string{"src/api/h.go"}, Read, 60, now); len(c) != 1 {
		t.Fatalf("read vs write = %v", c)
	}
	// All or nothing: docs/x.md was not granted.
	if h := tb.Held("b"); len(h) != 0 {
		t.Fatalf("partial grant: %v", h)
	}
	// The holder's own leases never conflict; re-acquiring updates in place.
	if c := tb.Acquire("a", []string{"src/api/**", "src/api/h.go"}, Read, 30, now); c != nil {
		t.Fatalf("own overlap: %v", c)
	}
	if h := tb.Held("a"); len(h) != 2 || h[0].Mode != Read || h[0].TTL != 30 {
		t.Fatalf("held = %v", h)
	}
	// Reads share.
	if c := tb.Acquire("b", []string{"src/api/**"}, Read, 60, now); c != nil {
		t.Fatalf("shared read: %v", c)
	}
	if c := tb.Conflicts("c", []string{"src/**"}, Write); len(c) != 3 {
		t.Fatalf("write over readers = %v", c)
	}

	if r := tb.Release("a", []string{"src/api/h.go"}); len(r) != 1 {
		t.Fatalf("release one = %v", r)
	}
	if r := tb.Release("a", nil); len(r) != 1 {
		t.Fatalf("release all = %v", r)
	}
	if l := tb.List(); len(l) != 1 || l[0].Holder != "b" {
		t.Fatalf("list = %v", l)
	}
}
