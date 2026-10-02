package settings

import (
	"context"
	"testing"
)

// TestKeyTablesAgree pins the three views of the settings key space to each
// other: AllSpecs (canonical table), AllKeys (derived: value-bearing specs),
// and loadPairs (the read path). A new setting added to one but not the
// others fails here instead of silently disappearing from Load or from
// GET /api/settings (whose omission makes remote TUIs fall back to
// defaults).
func TestKeyTablesAgree(t *testing.T) {
	keys := AllKeys()
	keySet := map[string]bool{}
	for _, k := range keys {
		if keySet[k] {
			t.Errorf("AllKeys has duplicate key %q", k)
		}
		keySet[k] = true
	}

	// Every value-bearing Spec is in AllKeys (by construction) and every
	// KindAction is not.
	for _, s := range AllSpecs() {
		if s.Kind == KindAction {
			if keySet[s.Key] {
				t.Errorf("action spec %q must not appear in AllKeys", s.Key)
			}
			continue
		}
		if !keySet[s.Key] {
			t.Errorf("spec %q missing from AllKeys", s.Key)
		}
	}

	// loadPairs covers exactly the AllKeys set.
	var out Settings
	pairSet := map[string]bool{}
	for _, p := range loadPairs(&out) {
		if pairSet[p.key] {
			t.Errorf("loadPairs has duplicate key %q", p.key)
		}
		pairSet[p.key] = true
	}
	for k := range keySet {
		if !pairSet[k] {
			t.Errorf("AllKeys key %q has no loadPairs entry — Load would ignore it", k)
		}
	}
	for k := range pairSet {
		if !keySet[k] {
			t.Errorf("loadPairs key %q is not in AllKeys — GET /api/settings would omit it", k)
		}
	}
}

// mapStore is an in-memory settings.Store for Load tests.
type mapStore map[string]string

func (m mapStore) GetSetting(_ context.Context, key string) (string, error) { return m[key], nil }
func (m mapStore) SetSetting(_ context.Context, key, value string) error {
	m[key] = value
	return nil
}
func (m mapStore) AllSettings(_ context.Context) (map[string]string, error) { return m, nil }

func TestLoadUsesAllSettings(t *testing.T) {
	st := mapStore{
		"approve_enabled": "0",
		"tail_budget_kb":  "512",
		"layout_mode":     "vertical",
	}
	s, err := Load(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if s.ApproveEnabled {
		t.Error("ApproveEnabled should be false")
	}
	if s.TailBudgetKB != 512 {
		t.Errorf("TailBudgetKB = %d, want 512", s.TailBudgetKB)
	}
	if s.LayoutMode != "vertical" {
		t.Errorf("LayoutMode = %q, want vertical", s.LayoutMode)
	}
	// Missing keys fall back to defaults.
	if !s.SummaryEnabled {
		t.Error("SummaryEnabled should default to true")
	}
}

// TestPaneListPct: the legacy "30/70" enum migrates to 30 once, and
// out-of-range stored values are clamped so neither pane can vanish.
func TestPaneListPct(t *testing.T) {
	ctx := context.Background()
	st := mapStore{"pane_split": "30/70"}
	s, _ := Load(ctx, st)
	if s.PaneListPct != 30 || st["pane_list_pct"] != "30" {
		t.Fatalf("legacy 30/70: pct=%d stored=%q", s.PaneListPct, st["pane_list_pct"])
	}
	if s, _ := Load(ctx, mapStore{"pane_split": "50/50"}); s.PaneListPct != 50 {
		t.Fatalf("legacy 50/50: pct=%d", s.PaneListPct)
	}
	for v, want := range map[string]int{"3": 10, "99": 90, "junk": 50, "40": 40} {
		if s, _ := Load(ctx, mapStore{"pane_list_pct": v}); s.PaneListPct != want {
			t.Errorf("stored %q: pct=%d, want %d", v, s.PaneListPct, want)
		}
	}
	st = mapStore{}
	s, _ = Set(ctx, st, Defaults(), "pane_list_pct", 0)
	if s.PaneListPct != 10 || st["pane_list_pct"] != "10" {
		t.Fatalf("Set(0): pct=%d stored=%q", s.PaneListPct, st["pane_list_pct"])
	}
}
