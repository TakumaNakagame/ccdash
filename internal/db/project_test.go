package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// TestProjects: assigning a project creates it with a color, the color
// comes back on the session row, and a second project gets another color.
func TestProjects(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: "/" + id, Status: model.StatusStopped}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SetProject(ctx, "a", "web"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetProject(ctx, "b", "web"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetProject(ctx, "c", "infra"); err != nil {
		t.Fatal(err)
	}
	a, _, _ := d.GetSession(ctx, "a")
	b, _, _ := d.GetSession(ctx, "b")
	c, _, _ := d.GetSession(ctx, "c")
	if a.Project != "web" || a.ProjectColor == "" || a.ProjectColor != b.ProjectColor {
		t.Fatalf("a=%q/%q b=%q/%q", a.Project, a.ProjectColor, b.Project, b.ProjectColor)
	}
	if c.ProjectColor == "" || c.ProjectColor == a.ProjectColor {
		t.Fatalf("infra color %q vs web %q", c.ProjectColor, a.ProjectColor)
	}
	if err := d.SetProjectColor(ctx, "web", ColorRandom); err != nil {
		t.Fatal(err)
	}
	a2, _, _ := d.GetSession(ctx, "a")
	if a2.ProjectColor == a.ProjectColor {
		t.Fatalf("re-roll kept %s", a.ProjectColor)
	}
	if err := d.SetProject(ctx, "a", ""); err != nil {
		t.Fatal(err)
	}
	a3, _, _ := d.GetSession(ctx, "a")
	if a3.Project != "" || a3.ProjectColor != "" {
		t.Fatalf("removed: %q/%q", a3.Project, a3.ProjectColor)
	}
}

// TestAutoArchive: old, idle-or-stopped sessions get archived except
// favorites, project members and running ones; a resumed one comes back,
// a manually archived one does not.
func TestAutoArchive(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	old := time.Now().AddDate(0, 0, -10)
	for _, s := range []model.Session{
		{SessionID: "old", Status: model.StatusStopped, LastSeen: old},
		{SessionID: "fav", Status: model.StatusStopped, LastSeen: old},
		{SessionID: "proj", Status: model.StatusStopped, LastSeen: old},
		{SessionID: "run", Status: model.StatusIdle, LastSeen: old},
		{SessionID: "new", Status: model.StatusStopped},
		{SessionID: "manual", Status: model.StatusStopped},
	} {
		s.Cwd = "/x"
		if err := d.UpsertSession(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.SetFavorite(ctx, "fav", true)
	_ = d.SetProject(ctx, "proj", "p")
	_ = d.SetArchived(ctx, "manual", true)
	n, err := d.AutoArchive(ctx, time.Now().AddDate(0, 0, -7))
	if err != nil || n != 1 {
		t.Fatalf("AutoArchive = %d, %v; want 1", n, err)
	}
	arch := func(id string) bool { s, _, _ := d.GetSession(ctx, id); return s.Archived }
	if !arch("old") || arch("fav") || arch("proj") || arch("run") || arch("new") {
		t.Fatal("wrong rows archived")
	}
	// Discovery re-reporting the same last_seen keeps it archived.
	if err := d.UpsertDiscoveredSession(ctx, &model.Session{SessionID: "old", Cwd: "/x", Status: model.StatusStopped, LastSeen: old}); err != nil {
		t.Fatal(err)
	}
	if !arch("old") {
		t.Fatal("unchanged rediscovery unarchived it")
	}
	// Resumed (running again): back in the list.
	if err := d.UpsertDiscoveredSession(ctx, &model.Session{SessionID: "old", Cwd: "/x", Status: model.StatusIdle, LastSeen: old}); err != nil {
		t.Fatal(err)
	}
	if arch("old") {
		t.Fatal("resumed session stayed archived")
	}
	// A manual archive stays put on activity.
	if err := d.UpsertSession(ctx, &model.Session{SessionID: "manual", Cwd: "/x", Status: model.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if !arch("manual") {
		t.Fatal("manual archive was undone")
	}
}
