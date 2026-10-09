package server

import (
	"context"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/discovery"
	"github.com/takumanakagame/ccmanage/internal/model"
)

// TestMaybeAutoTitle: only sessions past their first exchange or two are
// kicked off, once, and the run is rate-limited.
func TestMaybeAutoTitle(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no claude binary: the batch fails fast
	s, d, _ := newTestServer(t)
	ctx := context.Background()
	now := time.Now()
	for _, x := range []struct {
		id      string
		prompts int
		quiet   time.Duration
	}{
		{"two", 2, 0},
		{"one-quiet", 1, 5 * time.Minute},
		{"one-busy", 1, 0},
		{"none", 0, time.Hour},
	} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: x.id, Cwd: "/w", TranscriptPath: "/w/" + x.id + ".jsonl", Status: model.StatusIdle}); err != nil {
			t.Fatal(err)
		}
		s.titler.note(discovery.Discovered{SessionID: x.id, Prompts: x.prompts, LastModified: now.Add(-x.quiet)})
	}
	s.maybeAutoTitle(ctx, now)
	for id, want := range map[string]bool{"two": true, "one-quiet": true, "one-busy": false, "none": false} {
		got, _, _ := d.GetSession(ctx, id)
		if (got.TitleStatus != "") != want {
			t.Errorf("%s: title_status %q, kicked = %v want %v", id, got.TitleStatus, got.TitleStatus != "", want)
		}
	}
	// Within the minute nothing else starts, even once one-busy is ready.
	s.titler.note(discovery.Discovered{SessionID: "one-busy", Prompts: 2, LastModified: now})
	s.maybeAutoTitle(ctx, now.Add(10*time.Second))
	if got, _, _ := d.GetSession(ctx, "one-busy"); got.TitleStatus != "" {
		t.Fatal("ran again within autoTitleEvery")
	}
	s.maybeAutoTitle(ctx, now.Add(2*time.Minute))
	if got, _, _ := d.GetSession(ctx, "one-busy"); got.TitleStatus == "" {
		t.Fatal("ready session not titled on the next run")
	}
}

// TestApplyPTYProjects: a spawn started for a project joins it once its
// session-ID alias and row exist; an existing project is kept.
func TestApplyPTYProjects(t *testing.T) {
	s, d, _ := newTestServer(t)
	ctx := context.Background()
	fresh := &ptyEntry{project: "web"}
	kept := &ptyEntry{project: "web"}
	s.ptyMap["pid-1"] = fresh
	s.ptyMap["pid-2"] = kept
	s.applyPTYProjects(ctx) // no alias yet: nothing to do
	if fresh.projectDone {
		t.Fatal("applied before the alias")
	}
	s.ptyMap["new-sid"] = fresh
	s.ptyMap["old-sid"] = kept
	s.applyPTYProjects(ctx) // alias but no row yet
	if fresh.projectDone {
		t.Fatal("applied before the row")
	}
	for _, id := range []string{"new-sid", "old-sid"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: "/w", Status: model.StatusIdle}); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.SetProject(ctx, "old-sid", "infra")
	s.applyPTYProjects(ctx)
	if got, _, _ := d.GetSession(ctx, "new-sid"); got.Project != "web" || !fresh.projectDone {
		t.Fatalf("new-sid project = %q", got.Project)
	}
	if got, _, _ := d.GetSession(ctx, "old-sid"); got.Project != "infra" {
		t.Fatalf("old-sid project overwritten: %q", got.Project)
	}
}
