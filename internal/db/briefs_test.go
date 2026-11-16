package db

import (
	"context"
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
