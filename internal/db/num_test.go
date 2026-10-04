package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
)

func nums(t *testing.T, d *DB) map[string]int64 {
	t.Helper()
	ss, err := d.ListSessions(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, s := range ss {
		out[s.SessionID] = s.Num
	}
	return out
}

func TestSessionNums(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"a", "b", "c"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: "/x", Status: model.StatusIdle,
			FirstSeen: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	// Re-upserting an existing row must keep its number.
	if err := d.UpsertSession(ctx, &model.Session{SessionID: "a", Cwd: "/y", Status: model.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if got := nums(t, d); got["a"] != 1 || got["b"] != 2 || got["c"] != 3 {
		t.Fatalf("nums = %v, want a=1 b=2 c=3", got)
	}

	// A pre-num database: rows without a number get the next ones in
	// first-seen order on the next Open, existing numbers untouched.
	if _, err := d.sql.Exec(`UPDATE sessions SET num = NULL WHERE session_id IN ('b','c')`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got := nums(t, d); got["a"] != 1 || got["b"] != 2 || got["c"] != 3 {
		t.Fatalf("after backfill nums = %v, want a=1 b=2 c=3", got)
	}
	if err := d.UpsertSession(ctx, &model.Session{SessionID: "d", Cwd: "/x", Status: model.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	if got := nums(t, d)["d"]; got != 4 {
		t.Fatalf("new row num = %d, want 4", got)
	}
}

func TestGenTitleRoundTrip(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := d.UpsertSession(ctx, &model.Session{SessionID: "a", Cwd: "/x", Title: "first prompt", Status: model.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetTitleStatus(ctx, []string{"a"}, "running"); err != nil {
		t.Fatal(err)
	}
	if err := d.SweepRunningTitles(ctx); err != nil {
		t.Fatal(err)
	}
	s, _, _ := d.GetSession(ctx, "a")
	if s.TitleStatus != "error" {
		t.Fatalf("sweep: title_status = %q, want error", s.TitleStatus)
	}
	if err := d.SetGenTitle(ctx, "a", "ccdash 改善"); err != nil {
		t.Fatal(err)
	}
	s, _, _ = d.GetSession(ctx, "a")
	if s.GenTitle != "ccdash 改善" || s.TitleStatus != "done" || s.GenTitleAt.IsZero() || s.Num != 1 {
		t.Fatalf("got %+v", s)
	}
	if s.DisplayTitle() != "ccdash 改善" {
		t.Errorf("DisplayTitle = %q, want the generated title over the first prompt", s.DisplayTitle())
	}
	if err := d.SetCustomTitle(ctx, "a", "mine"); err != nil {
		t.Fatal(err)
	}
	s, _, _ = d.GetSession(ctx, "a")
	if s.DisplayTitle() != "mine" {
		t.Errorf("DisplayTitle = %q, want the rename to win", s.DisplayTitle())
	}
}
