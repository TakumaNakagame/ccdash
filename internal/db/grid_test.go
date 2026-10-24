package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

func TestSetGrid(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: "/x", Status: model.StatusIdle}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(id string) model.Session {
		t.Helper()
		s, ok, err := d.GetSession(ctx, id)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		return s
	}
	if s := get("a"); s.Grid {
		t.Fatal("on the grid before anyone put it there")
	}
	if err := d.SetGrid(ctx, "a", true); err != nil {
		t.Fatal(err)
	}
	first := get("a").GridAt
	if !get("a").Grid || first.IsZero() {
		t.Fatal("SetGrid(true) didn't stick")
	}
	// Marking again keeps the original time (the tile keeps its place).
	time.Sleep(5 * time.Millisecond)
	_ = d.SetGrid(ctx, "a", true)
	if got := get("a").GridAt; !got.Equal(first) {
		t.Fatalf("re-mark moved grid_at %v → %v", first, got)
	}
	ss, err := d.ListSessions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.Grid != (s.SessionID == "a") {
			t.Fatalf("list: %s grid=%v", s.SessionID, s.Grid)
		}
	}
	if err := d.SetGrid(ctx, "a", false); err != nil {
		t.Fatal(err)
	}
	if s := get("a"); s.Grid || !s.GridAt.IsZero() {
		t.Fatalf("still on the grid: %+v", s)
	}
}
