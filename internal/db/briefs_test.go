package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// TestParent: a spawn's parent is set once; adopting overwrites, releasing
// clears; both listings carry it.
func TestParent(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	for _, id := range []string{"boss", "boss2", "kid"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: "/w", Status: model.StatusIdle}); err != nil {
			t.Fatal(err)
		}
	}
	parent := func() (string, string) {
		t.Helper()
		x, ok, err := d.GetSession(ctx, "kid")
		if err != nil || !ok {
			t.Fatalf("get: %v %v", ok, err)
		}
		return x.Parent, x.ParentHost
	}
	if err := d.SetParent(ctx, "kid", "boss", "dev-1"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetParent(ctx, "kid", "boss2", ""); err != nil { // the first sticks
		t.Fatal(err)
	}
	if p, h := parent(); p != "boss" || h != "dev-1" {
		t.Errorf("after two SetParent: %q %q", p, h)
	}
	ss, err := d.ListSessions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range ss {
		if x.SessionID == "kid" {
			found = x.Parent == "boss" && x.ParentHost == "dev-1"
		}
	}
	if !found {
		t.Errorf("ListSessions lacks the parent: %+v", ss)
	}
	if err := d.AdoptParent(ctx, "kid", "boss2", ""); err != nil { // adopt overwrites
		t.Fatal(err)
	}
	if p, h := parent(); p != "boss2" || h != "" {
		t.Errorf("after adopt: %q %q", p, h)
	}
	if err := d.AdoptParent(ctx, "kid", "", "dev-9"); err != nil { // release clears both
		t.Fatal(err)
	}
	if p, h := parent(); p != "" || h != "" {
		t.Errorf("after release: %q %q", p, h)
	}

	// The my_sessions bookmark only moves forward.
	t1 := time.UnixMilli(1_700_000_000_500).UTC()
	_ = d.SetChildrenSeen(ctx, "boss", t1)
	_ = d.SetChildrenSeen(ctx, "boss", t1.Add(-time.Hour))
	if x, _, _ := d.GetSession(ctx, "boss"); !x.ChildrenSeenAt.Equal(t1) {
		t.Errorf("children_seen_at = %v, want %v", x.ChildrenSeenAt, t1)
	}
}

// TestBriefs: a brief is written before its session is known and linked
// once; a second link doesn't move it.
func TestBriefs(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	id, err := d.InsertBrief(ctx, model.Brief{PtyKey: "pid-7", Parent: "boss", Project: "web", Cwd: "/w/web", Text: "build the login page"})
	if err != nil {
		t.Fatal(err)
	}
	if bs, err := d.SessionBriefs(ctx, "kid"); err != nil || len(bs) != 0 {
		t.Fatalf("before link: %v %v", bs, err)
	}
	if err := d.LinkBrief(ctx, id, "kid"); err != nil {
		t.Fatal(err)
	}
	if err := d.LinkBrief(ctx, id, "other"); err != nil {
		t.Fatal(err)
	}
	bs, err := d.SessionBriefs(ctx, "kid")
	if err != nil || len(bs) != 1 {
		t.Fatalf("after link: %v %v", bs, err)
	}
	b := bs[0]
	if b.Text != "build the login page" || b.Parent != "boss" || b.Project != "web" || b.Cwd != "/w/web" || b.PtyKey != "pid-7" || b.CreatedAt.IsZero() {
		t.Errorf("brief = %+v", b)
	}
	if bs, _ := d.SessionBriefs(ctx, "other"); len(bs) != 0 {
		t.Errorf("relinked: %v", bs)
	}
}

// TestProjectNotes: a project's directory and note, the common note, and
// what a brief records about the notes appended to it.
func TestProjectNotes(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	dir := t.TempDir()

	// Setting a note or a directory makes the project when it is new.
	if err := d.SetProjectBrief(ctx, " web ", "  PR は main へ\r\n確認してからマージ \n"); err != nil {
		t.Fatal(err)
	}
	if got, err := d.SetProjectDir(ctx, "web", dir+"/./"); err != nil || got != dir {
		t.Fatalf("SetProjectDir = %q, %v", got, err)
	}
	gotDir, gotBrief, err := d.ProjectStart(ctx, "web")
	if err != nil || gotDir != dir || gotBrief != "PR は main へ\n確認してからマージ" {
		t.Fatalf("ProjectStart = %q %q %v", gotDir, gotBrief, err)
	}
	if a, b, err := d.ProjectStart(ctx, "nope"); a != "" || b != "" || err != nil {
		t.Errorf("unknown project = %q %q %v", a, b, err)
	}
	for _, bad := range []string{"rel/dir", dir + "/missing"} {
		if _, err := d.SetProjectDir(ctx, "web", bad); err == nil {
			t.Errorf("SetProjectDir(%q): want error", bad)
		}
	}
	if _, err := d.SetProjectDir(ctx, "", dir); err == nil {
		t.Error("no project: want error")
	}
	if err := d.SetProjectBrief(ctx, "web", strings.Repeat("あ", model.BriefMax)); err == nil {
		t.Error("oversized note: want error")
	}

	// The list carries them, and where the newest session ran.
	for i, cwd := range []string{"/w/old", "/w/new"} {
		x := model.Session{SessionID: "s" + string(rune('a'+i)), Cwd: cwd, Status: model.StatusIdle, LastSeen: time.Unix(int64(1000+i), 0)}
		if err := d.UpsertSession(ctx, &x); err != nil {
			t.Fatal(err)
		}
		if err := d.SetProject(ctx, x.SessionID, "web"); err != nil {
			t.Fatal(err)
		}
	}
	ps, err := d.ListProjects(ctx)
	if err != nil || len(ps) != 1 {
		t.Fatalf("ListProjects = %+v %v", ps, err)
	}
	if p := ps[0]; p.Dir != dir || p.Brief == "" || p.RecentDir != "/w/new" {
		t.Errorf("project row = %+v", p)
	}

	// Merging onto a project keeps its own values and takes what it lacks.
	if err := d.SetProjectBrief(ctx, "site", "site の注意"); err != nil {
		t.Fatal(err)
	}
	if err := d.RenameProject(ctx, "web", "site"); err != nil {
		t.Fatal(err)
	}
	if gotDir, gotBrief, _ := d.ProjectStart(ctx, "site"); gotDir != dir || gotBrief != "site の注意" {
		t.Errorf("after merge: %q %q", gotDir, gotBrief)
	}
	// "" clears.
	if _, err := d.SetProjectDir(ctx, "site", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.SetProjectBrief(ctx, "site", ""); err != nil {
		t.Fatal(err)
	}
	if a, b, _ := d.ProjectStart(ctx, "site"); a != "" || b != "" {
		t.Errorf("after clear: %q %q", a, b)
	}

	if err := d.SetBriefCommon(ctx, "応答は日本語\n"); err != nil {
		t.Fatal(err)
	}
	if c, _ := d.BriefCommon(ctx); c != "応答は日本語" {
		t.Errorf("common = %q", c)
	}

	id, err := d.InsertBrief(ctx, model.Brief{SessionID: "sa", Text: "do it\n\n## 共通の注意\n\n応答は日本語", Notes: []string{model.NoteCommon}, OwnLen: 5})
	if err != nil || id == 0 {
		t.Fatal(err)
	}
	if _, err := d.InsertBrief(ctx, model.Brief{SessionID: "sa", Text: "plain"}); err != nil {
		t.Fatal(err)
	}
	bs, err := d.SessionBriefs(ctx, "sa")
	if err != nil || len(bs) != 2 {
		t.Fatalf("briefs = %+v %v", bs, err)
	}
	if b := bs[0]; b.OwnLen != 5 || len(b.Notes) != 1 || b.Notes[0] != "common" || b.Text[:b.OwnLen] != "do it" {
		t.Errorf("brief with notes = %+v", b)
	}
	if b := bs[1]; b.OwnLen != 0 || b.Notes != nil {
		t.Errorf("plain brief = %+v", b)
	}
}
