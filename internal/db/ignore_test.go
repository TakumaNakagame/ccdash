package db

import (
	"context"
	"strings"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// TestPruneCandidates: leftover automated rows are headless ones with no
// transcript on disk or under an ignored folder; interactive, running,
// ccdash-started and operator-kept rows are never listed. IgnoreSession
// removes the row with its events and remembers the ID.
func TestPruneCandidates(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	add := func(id, cwd, ep, tp string, st model.SessionStatus) {
		t.Helper()
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: cwd, Entrypoint: ep, TranscriptPath: tp, Status: st}); err != nil {
			t.Fatal(err)
		}
	}
	add("mcp-list", "/home/u", "sdk-cli", "/gone.jsonl", model.StatusStopped)
	add("nudge", "/home/u/nudge", "sdk-cli", "/there.jsonl", model.StatusStopped)
	add("headless-ok", "/home/u/p", "sdk-cli", "/there.jsonl", model.StatusStopped)
	add("chat", "/home/u", "cli", "/gone.jsonl", model.StatusStopped)
	add("moved", "/home/u", "sdk-cli", "/gone.jsonl", model.StatusStopped)
	if err := d.UpsertSession(ctx, &model.Session{SessionID: "moved", Cwd: "/home/u", Title: "a real prompt", Status: model.StatusStopped}); err != nil {
		t.Fatal(err)
	}
	add("running", "/home/u", "sdk-cli", "/gone.jsonl", model.StatusActive)
	add("pinned", "/home/u", "sdk-cli", "/gone.jsonl", model.StatusStopped)
	add("spawned", "/home/u", "sdk-cli", "/gone.jsonl", model.StatusStopped)
	if err := d.SetFavorite(ctx, "pinned", true); err != nil {
		t.Fatal(err)
	}
	if err := d.SetOrigin(ctx, "spawned", "grok"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AppendEvent(ctx, &model.Event{SessionID: "mcp-list", EventType: model.EventSessionEnd, Payload: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	rows, err := d.PruneCandidates(ctx,
		func(cwd string) bool { return cwd == "/home/u/nudge" },
		func(p string) bool { return p == "/there.jsonl" })
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.SessionID+":"+r.Why)
	}
	if want := "mcp-list:no transcript,nudge:ignored folder"; strings.Join(got, ",") != want {
		t.Fatalf("candidates = %v, want %s", got, want)
	}
	for _, r := range rows {
		if ok, err := d.IgnoreSession(ctx, r.SessionID); err != nil || !ok {
			t.Fatalf("ignore %s: %v %v", r.SessionID, ok, err)
		}
	}
	if d.SessionExists(ctx, "mcp-list") || !d.IsIgnored(ctx, "mcp-list") {
		t.Fatal("row not removed / not remembered")
	}
	if ev, _ := d.ListEvents(ctx, "mcp-list", 10); len(ev) != 0 {
		t.Fatalf("events left: %v", ev)
	}
	if ok, _ := d.IgnoreSession(ctx, "pinned"); ok || !d.SessionExists(ctx, "pinned") {
		t.Fatal("a pinned row was forgotten")
	}
}
