package board

import (
	"testing"
	"time"
)

func TestBoard(t *testing.T) {
	b := New()
	now := time.Unix(100, 0).UTC()
	if _, err := b.Set("", "x", "w1", now); err == nil {
		t.Error("empty key accepted")
	}
	b.Set("api/schema", "v1", "w1", now)
	b.Set("conv/naming", "camel", "w2", now)
	e, _ := b.Set("api/schema", "v2", "w2", now)
	if e.Version != 2 || e.Value != "v2" {
		t.Fatalf("overwrite = %+v", e)
	}
	if got, ok := b.Get("api/schema"); !ok || got.Value != "v2" || got.By != "w2" {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	if l := b.List("api/"); len(l) != 1 || l[0].Key != "api/schema" {
		t.Fatalf("List(api/) = %+v", l)
	}
	if l := b.List(""); len(l) != 2 || l[0].Key != "api/schema" || l[1].Key != "conv/naming" {
		t.Fatalf("List() = %+v", l)
	}

	b.Decide("JWT, not sessions", "w2", now)
	d, _ := b.Decide("Postgres", "lead", now)
	if d.N != 2 {
		t.Fatalf("second decision N = %d", d.N)
	}
	if _, err := b.Decide("", "x", now); err == nil {
		t.Error("empty decision accepted")
	}
	ds := b.Decisions()
	ds[0].Text = "mutated"
	if b.Decisions()[0].Text != "JWT, not sessions" {
		t.Fatal("Decisions exposed internal slice")
	}
}
