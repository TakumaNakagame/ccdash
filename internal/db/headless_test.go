package db

import (
	"context"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
)

// TestHeadlessHidden: a new `claude -p` row is filed auto-archived and
// stays so while it runs; an interactive resume brings it back; a manual
// unarchive sticks; with the setting off nothing is hidden.
func TestHeadlessHidden(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	archived := func(id string) bool {
		t.Helper()
		s, ok, err := d.GetSession(ctx, id)
		if err != nil || !ok {
			t.Fatalf("get %s: ok=%v err=%v", id, ok, err)
		}
		return s.Archived
	}
	up := func(s model.Session) {
		t.Helper()
		if err := d.UpsertSession(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	headless := func(id string, st model.SessionStatus) model.Session {
		return model.Session{SessionID: id, Cwd: "/w", Status: st, Entrypoint: "sdk-cli", HideHeadless: true}
	}

	up(headless("h", model.StatusActive))
	if !archived("h") {
		t.Fatal("new headless row not archived")
	}
	up(headless("h", model.StatusActive))                                  // still running
	up(model.Session{SessionID: "h", Cwd: "/w", Status: model.StatusIdle}) // a hook with no entrypoint
	if !archived("h") {
		t.Fatal("headless row unarchived while still headless")
	}
	up(model.Session{SessionID: "h", Cwd: "/w", Status: model.StatusActive, Entrypoint: "cli"})
	if archived("h") {
		t.Fatal("interactive resume did not bring the row back")
	}

	// Made by a hook that didn't know the entrypoint, classified later.
	up(model.Session{SessionID: "late", Cwd: "/w", Status: model.StatusActive})
	up(headless("late", model.StatusActive))
	if !archived("late") {
		t.Fatal("late-classified headless row not archived")
	}
	if err := d.SetArchived(ctx, "late", false); err != nil {
		t.Fatal(err)
	}
	up(headless("late", model.StatusIdle))
	if archived("late") {
		t.Fatal("a manual unarchive was undone")
	}

	// A row from before the rule (older than the window) is left alone.
	up(model.Session{SessionID: "old", Cwd: "/w", Status: model.StatusStopped, FirstSeen: time.Now().Add(-2 * headlessClassifyWindow)})
	up(headless("old", model.StatusStopped))
	if archived("old") {
		t.Fatal("old row archived retroactively")
	}

	// Setting off.
	s := headless("shown", model.StatusActive)
	s.HideHeadless = false
	up(s)
	if archived("shown") {
		t.Fatal("hide_headless off still archived")
	}
	// Interactive sessions never.
	up(model.Session{SessionID: "i", Cwd: "/w", Status: model.StatusActive, Entrypoint: "cli", HideHeadless: true})
	if archived("i") {
		t.Fatal("interactive row archived")
	}
}
