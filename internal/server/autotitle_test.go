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
