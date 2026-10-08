package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/takumanakagame/ccmanage/internal/model"
)

// TestRunningColors: running sessions get stored colors that keep apart,
// stopped ones are left alone, and a re-roll stays clear of the others.
func TestRunningColors(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, s := range []model.Session{
		{SessionID: "a", Cwd: "/a", Status: model.StatusActive},
		{SessionID: "b", Cwd: "/b", Status: model.StatusIdle},
		{SessionID: "c", Cwd: "/c", Status: model.StatusIdle},
		{SessionID: "old", Cwd: "/o", Status: model.StatusStopped},
	} {
		if err := d.UpsertSession(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AssignRunningColors(ctx); err != nil {
		t.Fatal(err)
	}
	colors := map[string]string{}
	for _, id := range []string{"a", "b", "c", "old"} {
		s, _, _ := d.GetSession(ctx, id)
		colors[id] = s.ColorOverride
	}
	if colors["old"] != "" {
		t.Errorf("stopped session got a color: %s", colors["old"])
	}
	if colors["a"] == "" || colors["a"] == colors["b"] || colors["b"] == colors["c"] || colors["a"] == colors["c"] {
		t.Fatalf("running colors not distinct: %v", colors)
	}

	for i := 0; i < 30; i++ {
		if err := d.SetColor(ctx, "a", ColorRandom); err != nil {
			t.Fatal(err)
		}
		s, _, _ := d.GetSession(ctx, "a")
		if s.ColorOverride == colors["b"] || s.ColorOverride == colors["c"] || !model.ValidColor(s.ColorOverride) {
			t.Fatalf("re-roll drew %q against b=%s c=%s", s.ColorOverride, colors["b"], colors["c"])
		}
	}
}
