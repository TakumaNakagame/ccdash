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

	// LayoutMode is one of "auto" / "vertical" / "horizontal". "auto" picks
	// vertical on narrow terminals so 4K-half windows do the right thing
	// without manual flag flipping.
	LayoutMode string
	// VerticalAutoCols is the terminal-width threshold (in columns) that
	// auto-mode uses to pick vertical. Below this width, layout flips
	// vertical; at or above, horizontal stays.
	VerticalAutoCols int

	// Risk-bearing capabilities. Each defaults to ON for parity with prior
	// behavior; the operator can flip them off individually or via the
	// "Apply secure preset" action on the settings page.
	ApproveEnabled  bool // PermissionRequest blocking + a/A/d shortcuts
	SummaryEnabled  bool // s key + claude -p spawn
	AttachEnabled   bool // Enter spawns claude --resume / tmux switch
	AutoInstallSync bool // server boot rewrites settings.json on token mismatch

	// WindowedAttach renders attach in the right pane via the vt10x
	// emulator instead of suspending Bubble Tea for a fullscreen
	// pass-through. Off by default — the emulator path doesn't yet handle
	// CJK / wide chars correctly so heavy Japanese input drifts. Operators
	// who want the side-panel UX can opt in from the settings page.
	WindowedAttach bool

	TailBudgetKB      int
	SummaryTimeoutSec int
	RefreshIntervalMs int
}

const (
	keyAutoRepoTabs      = "auto_repo_tabs"
	keyBellOnPending     = "bell_on_pending"
	keyNewestAtBottom    = "newest_at_bottom"
	keyLayoutMode        = "layout_mode"
	// KeyVerticalAutoCols is exported so the TUI can annotate this row
	// with the live terminal width.
	KeyVerticalAutoCols  = "vertical_auto_cols"
	keyVerticalAutoCols  = KeyVerticalAutoCols
	keyApproveEnabled    = "approve_enabled"

	// Legacy keys retained only for one-shot migration on Load.
	legacyKeyLayoutVertical = "layout_vertical"
	legacyKeyLayoutAuto     = "layout_auto"
	keySummaryEnabled    = "summary_enabled"
	keyAttachEnabled     = "attach_enabled"
	keyAutoInstallSync   = "auto_install_sync"
	keyWindowedAttach    = "windowed_attach"
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
		LayoutMode:        "auto",
		VerticalAutoCols:  100,
		ApproveEnabled:    true,
		SummaryEnabled:    true,
		AttachEnabled:     true,
		AutoInstallSync:   true,
		WindowedAttach:    false, // opt-in: vt10x has CJK render bugs

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
		{keyLayoutMode, func(v string) {
			switch v {
			case "auto", "vertical", "horizontal":
				out.LayoutMode = v
			}
		}},
		{keyVerticalAutoCols, func(v string) { out.VerticalAutoCols = parseInt(v, out.VerticalAutoCols) }},
		{keyApproveEnabled, func(v string) { out.ApproveEnabled = parseBool(v, out.ApproveEnabled) }},
		{keySummaryEnabled, func(v string) { out.SummaryEnabled = parseBool(v, out.SummaryEnabled) }},
		{keyAttachEnabled, func(v string) { out.AttachEnabled = parseBool(v, out.AttachEnabled) }},
		{keyAutoInstallSync, func(v string) { out.AutoInstallSync = parseBool(v, out.AutoInstallSync) }},
		{keyWindowedAttach, func(v string) { out.WindowedAttach = parseBool(v, out.WindowedAttach) }},
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
	// One-shot migration from the previous two-bool layout scheme. If the
	// new key is missing but the legacy ones are present, fold them into a
	// single mode so the operator doesn't lose their preference. We don't
	// delete the old rows so a downgrade still finds them.
	if cur, _ := d.GetSetting(ctx, keyLayoutMode); cur == "" {
		auto, _ := d.GetSetting(ctx, legacyKeyLayoutAuto)
		vert, _ := d.GetSetting(ctx, legacyKeyLayoutVertical)
		if auto != "" || vert != "" {
			mode := "auto"
			if !parseBool(auto, true) {
				if parseBool(vert, false) {
					mode = "vertical"
				} else {
					mode = "horizontal"
				}
			}
			out.LayoutMode = mode
			_ = d.SetSetting(ctx, keyLayoutMode, mode)
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
	// Options enumerate the legal values for KindEnum, in cycle order.
	Options []string
}

type Kind int

const (
	KindBool Kind = iota
	KindInt
	// KindAction is a "button" row on the settings page. The Apply func
	// runs when the operator activates it; the spec carries no value.
	KindAction
	// KindEnum cycles a string value through a fixed list of Options.
	KindEnum
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
		{Key: keyLayoutMode, Label: "Vertical layout", Help: "Auto = pick from terminal width (vertical when narrow). On = always vertical. Off = always horizontal (side-by-side).", Kind: KindEnum, Options: []string{"auto", "on", "off"}},
		{Key: keyVerticalAutoCols, Label: "Vertical auto threshold (cols)", Help: "Width in columns below which auto-layout flips to vertical. Lower = stay horizontal longer; higher = go vertical sooner.", Kind: KindInt, Min: 40, Max: 240},
		// Risk-bearing toggles
		{Key: keyApproveEnabled, Label: "Approval blocking", Help: "When OFF, ccdash never holds PermissionRequest hooks — Claude prompts you in the terminal as it would without ccdash, and the a/A/d shortcuts are disabled", Kind: KindBool},
		{Key: keySummaryEnabled, Label: "Summarize via claude -p", Help: "When OFF, the 's' key is disabled and ccdash never spawns claude -p (no transcript digests sent over the network)", Kind: KindBool},
		{Key: keyAttachEnabled, Label: "Attach (enter)", Help: "When OFF, Enter only shows session info — ccdash never spawns claude --resume or runs tmux switch-client", Kind: KindBool},
		{Key: keyAutoInstallSync, Label: "Auto-rewrite settings.json", Help: "When OFF, server start does NOT silently rewrite ~/.claude/settings.json when the token rotates; you'll need to run install-hooks manually", Kind: KindBool},
		{Key: keyWindowedAttach, Label: "Windowed attach (experimental)", Help: "When ON, Enter renders claude inside ccdash's right pane via a vt10x emulator (Ctrl+F to fullscreen, Ctrl+D to detach). When OFF (default), Enter suspends Bubble Tea and hands the whole terminal to claude. The windowed path doesn't yet handle CJK / wide chars correctly, so heavy Japanese input drifts.", Kind: KindBool},
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
	case keyLayoutMode:
		// Surface the field as the user-facing label ("on"/"off") even
		// though we store "vertical"/"horizontal" internally. The Set
		// path translates back.
		switch s.LayoutMode {
		case "vertical":
			return "on"
		case "horizontal":
			return "off"
		default:
			return "auto"
		}
	case keyVerticalAutoCols:
		return s.VerticalAutoCols
	case keyApproveEnabled:
		return s.ApproveEnabled
	case keySummaryEnabled:
		return s.SummaryEnabled
	case keyAttachEnabled:
		return s.AttachEnabled
	case keyAutoInstallSync:
		return s.AutoInstallSync
	case keyWindowedAttach:
		return s.WindowedAttach
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
	case keyLayoutMode:
		mode := value.(string)
		// Translate the user-facing labels to the canonical storage form.
		switch mode {
		case "on":
			mode = "vertical"
		case "off":
			mode = "horizontal"
		default:
			mode = "auto"
		}
		s.LayoutMode = mode
	case keyVerticalAutoCols:
		s.VerticalAutoCols = value.(int)
	case keyApproveEnabled:
		s.ApproveEnabled = value.(bool)
	case keySummaryEnabled:
		s.SummaryEnabled = value.(bool)
	case keyAttachEnabled:
		s.AttachEnabled = value.(bool)
	case keyAutoInstallSync:
		s.AutoInstallSync = value.(bool)
	case keyWindowedAttach:
		s.WindowedAttach = value.(bool)
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
	case string:
		// For KindEnum we may have done a label→canonical translation
		// inside Set(); re-translate here too so what hits disk matches
		// what we'd parse back in Load.
		if key == keyLayoutMode {
			switch v {
			case "on":
				v = "vertical"
			case "off":
				v = "horizontal"
			default:
				if v != "vertical" && v != "horizontal" {
					v = "auto"
				}
			}
		}
		return d.SetSetting(ctx, key, v)
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
