package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
)

func TestFillWork(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := d.UpsertSession(ctx, &model.Session{SessionID: "a", Cwd: "/x", Status: model.StatusActive}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Minute).Truncate(time.Second)
	ev := func(at time.Duration, typ model.EventType, tool, sum string) {
		t.Helper()
		if _, err := d.AppendEvent(ctx, &model.Event{SessionID: "a", Timestamp: t0.Add(at), EventType: typ, Tool: tool, Summary: sum}); err != nil {
			t.Fatal(err)
		}
	}
	get := func() model.Session {
		t.Helper()
		s, ok, err := d.GetSession(ctx, "a")
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		return s
	}
	ev(0, model.EventUserPrompt, "", "do it")
	ev(time.Second, model.EventPreTool, "Bash", "go test ./...")
	s := get()
	if !s.TurnStartedAt.Equal(t0) || s.Tool != "Bash" || s.ToolSummary != "go test ./..." || !s.ToolAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("in flight: %+v", s)
	}
	// Same second: the later row (by id) wins.
	ev(time.Second, model.EventPostTool, "Bash", "go test ./... (10ms)")
	if s := get(); s.Tool != "" {
		t.Fatalf("after post_tool: tool %q", s.Tool)
	}
	// A pre_tool from before the current turn is not in flight.
	ev(2*time.Second, model.EventPreTool, "Read", "/x/y")
	ev(3*time.Second, model.EventUserPrompt, "", "next")
	if s := get(); s.Tool != "" || !s.TurnStartedAt.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("new turn: %+v", s)
	}
	// Only active rows get it.
	ev(4*time.Second, model.EventPreTool, "Grep", "foo")
	if err := d.UpsertSession(ctx, &model.Session{SessionID: "a", Cwd: "/x", Status: model.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	ss, err := d.ListSessions(ctx, false)
	if err != nil || len(ss) != 1 {
		t.Fatal(ss, err)
	}
	if ss[0].Status == model.StatusIdle && (ss[0].Tool != "" || !ss[0].TurnStartedAt.IsZero()) {
		t.Fatalf("idle row: %+v", ss[0])
	}
}
