package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/takumanakagame/ccmanage/internal/db"
)

// Compile-time interface checks: both implementations must keep satisfying
// the full Store seam (a missing method otherwise only surfaces at the
// TUI/CLI call site).
var (
	_ Store = (*Local)(nil)
	_ Store = (*Remote)(nil)
)

func TestLocalAllSettings(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	l := NewLocal(d)
	ctx := context.Background()

	if err := l.SetSetting(ctx, "approve_enabled", "0"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetSetting(ctx, "tail_budget_kb", "512"); err != nil {
		t.Fatal(err)
	}

	all, err := l.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if all["approve_enabled"] != "0" || all["tail_budget_kb"] != "512" {
		t.Fatalf("AllSettings = %+v", all)
	}
	// Missing key: absent from the map — same as GetSetting's "".
	if v, ok := all["summary_enabled"]; ok && v != "" {
		t.Fatalf("unexpected summary_enabled value %q", v)
	}
	if v, err := l.GetSetting(ctx, "approve_enabled"); err != nil || v != "0" {
		t.Fatalf("GetSetting = %q, %v", v, err)
	}
}
