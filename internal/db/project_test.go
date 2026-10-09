package db

import (
	"context"
	"path/filepath"
	"strings"
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
	ps, err := d.ListProjects(ctx)
	if err != nil || len(ps) != 2 {
		t.Fatalf("ListProjects = %v, %v", ps, err)
	}
	for _, p := range ps {
		if want := map[string]int{"web": 2, "infra": 1}[p.Name]; p.Sessions != want || p.Color == "" {
			t.Fatalf("project %+v, want %d sessions", p, want)
		}
	}
	if err := d.SetProjectColor(ctx, "web", ColorRandom); err != nil {
		t.Fatal(err)
	}
	a2, _, _ := d.GetSession(ctx, "a")
	if a2.ProjectColor == a.ProjectColor {
		t.Fatalf("re-roll kept %s", a.ProjectColor)
	}
	// Order: infra was created last, so it comes first; moves reorder.
	order := func() string {
		ps, _ := d.ListProjects(ctx)
		var n []string
		for _, p := range ps {
			n = append(n, p.Name)
		}
		return strings.Join(n, ",")
	}
	if got := order(); got != "infra,web" {
		t.Fatalf("order = %s", got)
	}
	if err := d.MoveProject(ctx, "web", -1); err != nil || order() != "web,infra" {
		t.Fatalf("move: %v %s", err, order())
	}
	if err := d.SetProjectOrder(ctx, []string{"infra"}); err != nil || order() != "infra,web" {
		t.Fatalf("set order: %v %s", err, order())
	}
	if err := d.SetProjectOrder(ctx, []string{"nope"}); err == nil {
		t.Fatal("unknown project accepted")
	}
	// Rename, then merge into an existing project.
	if err := d.RenameProject(ctx, "infra", "ops"); err != nil {
		t.Fatal(err)
	}
	if c2, _, _ := d.GetSession(ctx, "c"); c2.Project != "ops" || c2.ProjectColor != c.ProjectColor {
		t.Fatalf("renamed: %q/%q", c2.Project, c2.ProjectColor)
	}
	if err := d.RenameProject(ctx, "ops", "web"); err != nil {
		t.Fatal(err)
	}
	if c3, _, _ := d.GetSession(ctx, "c"); c3.Project != "web" || c3.ProjectColor != a2.ProjectColor {
		t.Fatalf("merged: %q/%q", c3.Project, c3.ProjectColor)
	}
	if ps, _ := d.ListProjects(ctx); len(ps) != 1 {
		t.Fatalf("after merge: %+v", ps)
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

// TestPinOrder: pins come first, the most recently pinned first, whatever
// their activity; unpinning drops the stamp.
func TestPinOrder(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	now := time.Now()
	for i, id := range []string{"a", "b", "c"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: "/x", Status: model.StatusStopped, LastSeen: now.Add(-time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.SetFavorite(ctx, "c", true)
	time.Sleep(5 * time.Millisecond)
	_ = d.SetFavorite(ctx, "b", true)
	ss, _ := d.ListSessions(ctx, false)
	var got []string
	for _, s := range ss {
		got = append(got, s.SessionID)
	}
	if strings.Join(got, ",") != "b,c,a" {
		t.Fatalf("order = %v, want b,c,a", got)
	}
	if ss[0].PinnedAt.IsZero() || !ss[0].PinnedAt.After(ss[1].PinnedAt) {
		t.Fatalf("pinned_at: %v %v", ss[0].PinnedAt, ss[1].PinnedAt)
	}
	_ = d.SetFavorite(ctx, "b", false)
	if b, _, _ := d.GetSession(ctx, "b"); b.Favorite || !b.PinnedAt.IsZero() {
		t.Fatalf("unpinned: %+v", b)
	}
}
