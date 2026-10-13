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

// TestPaneRatio: the old pane_list_pct / pane_tasks_pct (and the older
// pane_split enum) fold into a ratio that draws the same screen, once;
// ratios parse loosely, normalize to 100 and keep every column ≥ 10%.
func TestPaneRatio(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		stored map[string]string
		want   string
	}{
		{map[string]string{}, "36:36:28"}, // fresh install: the default, nothing written
		{map[string]string{"pane_list_pct": "30"}, "21:51:28"},
		{map[string]string{"pane_list_pct": "50", "pane_tasks_pct": "25"}, "37:38:25"},
		{map[string]string{"pane_split": "30/70"}, "21:51:28"},
		{map[string]string{"pane_list_pct": "40", "pane_ratio": "20:60:20"}, "20:60:20"},
	} {
		st := mapStore(c.stored)
		s, _ := Load(ctx, st)
		if s.PaneRatio != c.want {
			t.Errorf("%v: ratio %q, want %q", c.stored, s.PaneRatio, c.want)
		}
		if _, had := c.stored["pane_ratio"]; !had && len(c.stored) > 0 && st["pane_ratio"] != c.want {
			t.Errorf("%v: not persisted (%q)", c.stored, st["pane_ratio"])
		}
	}
	// The migrated ratio keeps the old list share of list + work.
	if s, _ := Load(ctx, mapStore{"pane_list_pct": "30"}); s.Ratio().ListShare() != 29 {
		t.Errorf("list share = %d", s.Ratio().ListShare())
	}
	for in, want := range map[string]string{"1:2:1": "25:50:25", "30 / 40 / 30": "30:40:30", "1:100:1": "10:80:10", "50:50:50": "33:34:33"} {
		r, err := ParsePaneRatio(in)
		if err != nil || r.String() != want {
			t.Errorf("parse %q = %v %v, want %s", in, r, err, want)
		}
	}
	for _, bad := range []string{"", "1:2", "a:b:c", "0:1:1", "100:1:100"} {
		if _, err := ParsePaneRatio(bad); err == nil {
			t.Errorf("parse %q: want an error", bad)
		}
	}
	st := mapStore{}
	if _, err := Set(ctx, st, Defaults(), "pane_ratio", "x"); err == nil || st["pane_ratio"] != "" {
		t.Fatal("Set accepted a bad ratio")
	}
	s, _ := Set(ctx, st, Defaults(), "pane_ratio", "1:2:1")
	if s.PaneRatio != "25:50:25" || st["pane_ratio"] != "25:50:25" {
		t.Fatalf("Set: %q stored %q", s.PaneRatio, st["pane_ratio"])
	}
	if r := (PaneRatio{List: 12, Work: 60, Tasks: 28}).MoveList(-5); r.List != 10 || r.Work != 62 {
		t.Errorf("MoveList stops at the floor: %v", r)
	}
}
