// Package settings provides typed accessors over ccdash's persistent
// preferences (the settings table in SQLite). It centralizes the key
// names, default values, and value parsing in one place so the TUI and
// any future CLI surface read the same source of truth.
package settings

import (
	"context"
	"strconv"

	"github.com/takumanakagame/ccmanage/internal/db"
)

// Settings is the typed snapshot the TUI takes on startup. Field defaults
// are applied when the underlying row is missing or unparsable.
type Settings struct {
	AutoRepoTabs   bool
	BellOnPending  bool
	NewestAtBottom bool // session list with newest sessions at the bottom
	LayoutVertical bool // stack list above transcript instead of left/right
	LayoutAuto     bool // when true, ignore LayoutVertical and pick from terminal width

	// Risk-bearing capabilities. Each defaults to ON for parity with prior
	// behavior; the operator can flip them off individually or via the
	// "Apply secure preset" action on the settings page.
	ApproveEnabled  bool // PermissionRequest blocking + a/A/d shortcuts
	SummaryEnabled  bool // s key + claude -p spawn
	AttachEnabled   bool // Enter spawns claude --resume / tmux switch
	AutoInstallSync bool // server boot rewrites settings.json on token mismatch

	TailBudgetKB      int
	SummaryTimeoutSec int
	RefreshIntervalMs int
}

const (
	keyAutoRepoTabs      = "auto_repo_tabs"
	keyBellOnPending     = "bell_on_pending"
	keyNewestAtBottom    = "newest_at_bottom"
	keyLayoutVertical    = "layout_vertical"
	keyLayoutAuto        = "layout_auto"
	keyApproveEnabled    = "approve_enabled"
	keySummaryEnabled    = "summary_enabled"
	keyAttachEnabled     = "attach_enabled"
	keyAutoInstallSync   = "auto_install_sync"
	keyPresetSecure      = "preset_secure"
	keyTailBudgetKB      = "tail_budget_kb"
	keySummaryTimeoutSec = "summary_timeout_sec"
	keyRefreshIntervalMs = "refresh_interval_ms"
)

// Defaults returns the baseline values used whenever a key is missing.
func Defaults() Settings {
	return Settings{
		AutoRepoTabs:      true,
		BellOnPending:     true,
		NewestAtBottom:    false,
		LayoutVertical:    false,
		LayoutAuto:        true,
		ApproveEnabled:    true,
		SummaryEnabled:    true,
		AttachEnabled:     true,
		AutoInstallSync:   true,
		TailBudgetKB:      256,
		SummaryTimeoutSec: 180,
		RefreshIntervalMs: 1000,
	}
}

// Load reads every known key from the DB, falling back to Defaults() for
// each one that's missing or malformed. Always returns a populated
// Settings; the error is non-nil only on hard DB failures.
func Load(ctx context.Context, d *db.DB) (Settings, error) {
	out := Defaults()
	pairs := []struct {
		key string
		set func(string)
	}{
		{keyAutoRepoTabs, func(v string) { out.AutoRepoTabs = parseBool(v, out.AutoRepoTabs) }},
		{keyBellOnPending, func(v string) { out.BellOnPending = parseBool(v, out.BellOnPending) }},
		{keyNewestAtBottom, func(v string) { out.NewestAtBottom = parseBool(v, out.NewestAtBottom) }},
		{keyLayoutVertical, func(v string) { out.LayoutVertical = parseBool(v, out.LayoutVertical) }},
		{keyLayoutAuto, func(v string) { out.LayoutAuto = parseBool(v, out.LayoutAuto) }},
		{keyApproveEnabled, func(v string) { out.ApproveEnabled = parseBool(v, out.ApproveEnabled) }},
		{keySummaryEnabled, func(v string) { out.SummaryEnabled = parseBool(v, out.SummaryEnabled) }},
		{keyAttachEnabled, func(v string) { out.AttachEnabled = parseBool(v, out.AttachEnabled) }},
		{keyAutoInstallSync, func(v string) { out.AutoInstallSync = parseBool(v, out.AutoInstallSync) }},
		{keyTailBudgetKB, func(v string) { out.TailBudgetKB = parseInt(v, out.TailBudgetKB) }},
		{keySummaryTimeoutSec, func(v string) { out.SummaryTimeoutSec = parseInt(v, out.SummaryTimeoutSec) }},
		{keyRefreshIntervalMs, func(v string) { out.RefreshIntervalMs = parseInt(v, out.RefreshIntervalMs) }},
	}
	for _, p := range pairs {
		v, err := d.GetSetting(ctx, p.key)
		if err != nil {
			return out, err
		}
		if v != "" {
			p.set(v)
		}
	}
	return out, nil
}

func SetAutoRepoTabs(ctx context.Context, d *db.DB, v bool) error {
	return d.SetSetting(ctx, keyAutoRepoTabs, formatBool(v))
}

func SetBellOnPending(ctx context.Context, d *db.DB, v bool) error {
	return d.SetSetting(ctx, keyBellOnPending, formatBool(v))
}

func SetTailBudgetKB(ctx context.Context, d *db.DB, v int) error {
	return d.SetSetting(ctx, keyTailBudgetKB, strconv.Itoa(v))
}

func SetSummaryTimeoutSec(ctx context.Context, d *db.DB, v int) error {
	return d.SetSetting(ctx, keySummaryTimeoutSec, strconv.Itoa(v))
}

func SetRefreshIntervalMs(ctx context.Context, d *db.DB, v int) error {
	return d.SetSetting(ctx, keyRefreshIntervalMs, strconv.Itoa(v))
}

// Spec describes a single setting for UI rendering and validation.
type Spec struct {
	Key   string
	Label string
	Help  string
	Kind  Kind
	// Min / Max are only consulted when Kind == KindInt.
	Min, Max int
	// Apply is called when KindAction rows are activated.
	Apply ActionFunc
}

type Kind int

const (
	KindBool Kind = iota
	KindInt
	// KindAction is a "button" row on the settings page. The Apply func
	// runs when the operator activates it; the spec carries no value.
	KindAction
)

// Spec.Apply is non-nil only for KindAction rows.
type ActionFunc func(ctx context.Context, d *db.DB, s Settings) (Settings, error)

// AllSpecs returns every setting in display order. The TUI uses this both
// to render the modal page and to dispatch updates without a giant switch.
func AllSpecs() []Spec {
	return []Spec{
		{Key: keyAutoRepoTabs, Label: "Auto repo tabs", Help: "Include repo names in the Tab cycle alongside user-named tabs", Kind: KindBool},
		{Key: keyBellOnPending, Label: "Bell on pending", Help: "Ring the terminal bell when the pending count goes from 0 to >0", Kind: KindBool},
		{Key: keyNewestAtBottom, Label: "Newest at bottom", Help: "Show the newest session at the bottom of the list (matches the transcript tail orientation)", Kind: KindBool},
		{Key: keyLayoutAuto, Label: "Auto layout", Help: "Pick horizontal vs vertical from the terminal width — flips to vertical on narrow / portrait windows. Overrides the manual Vertical layout toggle below.", Kind: KindBool},
		{Key: keyLayoutVertical, Label: "Vertical layout (manual)", Help: "Force vertical layout even on wide terminals; only consulted when Auto layout is off", Kind: KindBool},
		// Risk-bearing toggles
		{Key: keyApproveEnabled, Label: "Approval blocking", Help: "When OFF, ccdash never holds PermissionRequest hooks — Claude prompts you in the terminal as it would without ccdash, and the a/A/d shortcuts are disabled", Kind: KindBool},
		{Key: keySummaryEnabled, Label: "Summarize via claude -p", Help: "When OFF, the 's' key is disabled and ccdash never spawns claude -p (no transcript digests sent over the network)", Kind: KindBool},
		{Key: keyAttachEnabled, Label: "Attach (enter)", Help: "When OFF, Enter only shows session info — ccdash never spawns claude --resume or runs tmux switch-client", Kind: KindBool},
		{Key: keyAutoInstallSync, Label: "Auto-rewrite settings.json", Help: "When OFF, server start does NOT silently rewrite ~/.claude/settings.json when the token rotates; you'll need to run install-hooks manually", Kind: KindBool},
		{Key: keyPresetSecure, Label: "Apply secure preset", Help: "Observation-only mode: turns off approval blocking, summarize, attach, and auto-install sync in one go", Kind: KindAction, Apply: applySecurePreset},
		// Numeric tunables
		{Key: keyTailBudgetKB, Label: "Right-pane tail budget (KB)", Help: "Bytes of transcript loaded for the inline live tail; bigger == more context, slower", Kind: KindInt, Min: 32, Max: 8192},
		{Key: keySummaryTimeoutSec, Label: "Summary timeout (s)", Help: "How long to wait for `claude -p` to produce a summary before giving up", Kind: KindInt, Min: 30, Max: 600},
		{Key: keyRefreshIntervalMs, Label: "Refresh interval (ms)", Help: "How often the TUI re-queries the DB for new state", Kind: KindInt, Min: 250, Max: 10000},
	}
}

// applySecurePreset turns off every risk-bearing capability in one shot.
// Convenience for operators who want pure observation without auditing each
// flag individually.
func applySecurePreset(ctx context.Context, d *db.DB, s Settings) (Settings, error) {
	for _, k := range []string{keyApproveEnabled, keySummaryEnabled, keyAttachEnabled, keyAutoInstallSync} {
		next, err := Set(ctx, d, s, k, false)
		if err != nil {
			return s, err
		}
		s = next
	}
	return s, nil
}

// Get returns the current value of one setting. The result is *any* — bool
// or int — and it's the caller's job to type-assert based on the spec's
// Kind. Used by the TUI page to render rows without a per-key switch.
func Get(s Settings, key string) any {
	switch key {
	case keyAutoRepoTabs:
		return s.AutoRepoTabs
	case keyBellOnPending:
		return s.BellOnPending
	case keyNewestAtBottom:
		return s.NewestAtBottom
	case keyLayoutVertical:
		return s.LayoutVertical
	case keyLayoutAuto:
		return s.LayoutAuto
	case keyApproveEnabled:
		return s.ApproveEnabled
	case keySummaryEnabled:
		return s.SummaryEnabled
	case keyAttachEnabled:
		return s.AttachEnabled
	case keyAutoInstallSync:
		return s.AutoInstallSync
	case keyTailBudgetKB:
		return s.TailBudgetKB
	case keySummaryTimeoutSec:
		return s.SummaryTimeoutSec
	case keyRefreshIntervalMs:
		return s.RefreshIntervalMs
	}
	return nil
}

// Set updates the in-memory snapshot AND persists the change. Returns the
// updated Settings so the caller can replace its copy in one line.
func Set(ctx context.Context, d *db.DB, s Settings, key string, value any) (Settings, error) {
	switch key {
	case keyAutoRepoTabs:
		s.AutoRepoTabs = value.(bool)
	case keyBellOnPending:
		s.BellOnPending = value.(bool)
	case keyNewestAtBottom:
		s.NewestAtBottom = value.(bool)
	case keyLayoutVertical:
		s.LayoutVertical = value.(bool)
	case keyLayoutAuto:
		s.LayoutAuto = value.(bool)
	case keyApproveEnabled:
		s.ApproveEnabled = value.(bool)
	case keySummaryEnabled:
		s.SummaryEnabled = value.(bool)
	case keyAttachEnabled:
		s.AttachEnabled = value.(bool)
	case keyAutoInstallSync:
		s.AutoInstallSync = value.(bool)
	case keyTailBudgetKB:
		s.TailBudgetKB = value.(int)
	case keySummaryTimeoutSec:
		s.SummaryTimeoutSec = value.(int)
	case keyRefreshIntervalMs:
		s.RefreshIntervalMs = value.(int)
	}
	return s, persist(ctx, d, key, value)
}

func persist(ctx context.Context, d *db.DB, key string, value any) error {
	switch v := value.(type) {
	case bool:
		return d.SetSetting(ctx, key, formatBool(v))
	case int:
		return d.SetSetting(ctx, key, strconv.Itoa(v))
	}
	return nil
}

func formatBool(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func parseBool(s string, fallback bool) bool {
	switch s {
	case "1", "true", "TRUE", "yes":
		return true
	case "0", "false", "FALSE", "no":
		return false
	}
	return fallback
}

func parseInt(s string, fallback int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return fallback
}
