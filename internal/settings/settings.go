// Package settings provides typed accessors over ccdash's persistent
// preferences (the settings table in SQLite). It centralizes the key
// names, default values, and value parsing in one place so the TUI and
// any future CLI surface read the same source of truth.
package settings

import (
	"context"
	"strconv"
)

// Store is the minimal capability Load/Set/ActionFunc need: read/write raw
// string setting values. internal/store.Store (Local and Remote alike)
// satisfies this automatically since its method set is a superset — this
// package deliberately does NOT import internal/store so that a bare *db.DB
// (which also has all three methods) can satisfy it too, without pulling the
// whole Store seam into the collector.
//
// Load goes through AllSettings — one bulk read — rather than per-key
// GetSetting calls, because in remote mode each GetSetting is a full HTTP
// round trip to the collector; per-key reads turned TUI startup into ~16
// sequential requests.
type Store interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
	AllSettings(ctx context.Context) (map[string]string, error)
}

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

	TailBudgetKB      int
	SummaryTimeoutSec int
	RefreshIntervalMs int

	// NewSessionDir is where the `n` directory picker starts. Empty means
	// the home directory. May use ~/.
	NewSessionDir string

	// PaneListPct is the session list's share of the body, in percent:
	// its width in the horizontal layout, its height in the vertical one.
	// The right (work) pane gets the rest. Clamped to
	// [MinPaneListPct, MaxPaneListPct] so neither pane can vanish.
	PaneListPct int
	// InvertListScroll flips the mouse wheel over the session list: wheel
	// down moves the selection up and vice versa. The right pane keeps
	// its normal direction.
	InvertListScroll bool
	// TimeFormat renders session times as "relative" (4m, 2h, 3d) or
	// "absolute" (09/30 18:45).
	TimeFormat string
}

const (
	keyAutoRepoTabs   = "auto_repo_tabs"
	keyBellOnPending  = "bell_on_pending"
	keyNewestAtBottom = "newest_at_bottom"
	keyLayoutMode     = "layout_mode"
	// KeyVerticalAutoCols is exported so the TUI can annotate this row
	// with the live terminal width.
	KeyVerticalAutoCols = "vertical_auto_cols"
	keyVerticalAutoCols = KeyVerticalAutoCols
	keyApproveEnabled   = "approve_enabled"

	// Legacy keys retained only for one-shot migration on Load.
	legacyKeyLayoutVertical = "layout_vertical"
	legacyKeyLayoutAuto     = "layout_auto"
	keySummaryEnabled       = "summary_enabled"
	keyAttachEnabled        = "attach_enabled"
	keyAutoInstallSync      = "auto_install_sync"
	keyPresetSecure         = "preset_secure"
	// KeyRestart is the "Restart ccdash" action row. It has no Apply:
	// the TUI intercepts it to show a confirmation modal and then exits
	// with tui.ErrRestart.
	KeyRestart = "restart"
	// KeyUpdate is the "Update ccdash" action row, also TUI-intercepted:
	// check for a release, show its notes, install, offer a restart.
	KeyUpdate            = "update"
	keyTailBudgetKB      = "tail_budget_kb"
	keySummaryTimeoutSec = "summary_timeout_sec"
	keyRefreshIntervalMs = "refresh_interval_ms"
	keyNewSessionDir     = "new_session_dir"
	keyPaneListPct       = "pane_list_pct"
	// legacyKeyPaneSplit was the "50/50" / "30/70" enum PaneListPct
	// replaced; Load folds it in once.
	legacyKeyPaneSplit  = "pane_split"
	keyTimeFormat       = "time_format"
	keyInvertListScroll = "invert_list_scroll"
)

// Defaults returns the baseline values used whenever a key is missing.
func Defaults() Settings {
	return Settings{
		AutoRepoTabs:     true,
		BellOnPending:    true,
		NewestAtBottom:   false,
		LayoutMode:       "auto",
		VerticalAutoCols: 100,
		ApproveEnabled:   true,
		SummaryEnabled:   true,
		AttachEnabled:    true,
		AutoInstallSync:  true,

		TailBudgetKB:      256,
		SummaryTimeoutSec: 180,
		RefreshIntervalMs: 1000,

		PaneListPct: 50,
		TimeFormat:  "relative",
	}
}

// loadPair binds a storage key to the setter that folds its raw value into
// a Settings snapshot. loadPairs is the counterpart of AllKeys/AllSpecs on
// the read path — the settings package test asserts its key set matches
// AllKeys exactly, so adding a Spec without a load pair (or vice versa)
// fails the build's test run instead of silently dropping the key from
// either Load or GET /api/settings.
type loadPair struct {
	key string
	set func(string)
}

func loadPairs(out *Settings) []loadPair {
	return []loadPair{
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
		{keyTailBudgetKB, func(v string) { out.TailBudgetKB = parseInt(v, out.TailBudgetKB) }},
		{keySummaryTimeoutSec, func(v string) { out.SummaryTimeoutSec = parseInt(v, out.SummaryTimeoutSec) }},
		{keyRefreshIntervalMs, func(v string) { out.RefreshIntervalMs = parseInt(v, out.RefreshIntervalMs) }},
		{keyNewSessionDir, func(v string) { out.NewSessionDir = v }},
		{keyPaneListPct, func(v string) { out.PaneListPct = ClampPaneListPct(parseInt(v, out.PaneListPct)) }},
		{keyInvertListScroll, func(v string) { out.InvertListScroll = parseBool(v, out.InvertListScroll) }},
		{keyTimeFormat, func(v string) {
			if v == "relative" || v == "absolute" {
				out.TimeFormat = v
			}
		}},
	}
}

// Load reads every known key from the store in one AllSettings call,
// falling back to Defaults() for each one that's missing or malformed.
// Always returns a populated Settings; the error is non-nil only on hard
// store failures.
func Load(ctx context.Context, st Store) (Settings, error) {
	out := Defaults()
	all, err := st.AllSettings(ctx)
	if err != nil {
		return out, err
	}
	for _, p := range loadPairs(&out) {
		if v := all[p.key]; v != "" {
			p.set(v)
		}
	}
	// One-shot migration from the previous two-bool layout scheme. If the
	// new key is missing but the legacy ones are present, fold them into a
	// single mode so the operator doesn't lose their preference. We don't
	// delete the old rows so a downgrade still finds them. The legacy keys
	// aren't part of AllKeys, so remote AllSettings maps won't carry them —
	// the migration then no-ops on remote clients and runs where the DB
	// actually lives (the collector's own settings.Load calls).
	if all[keyLayoutMode] == "" {
		auto, _ := st.GetSetting(ctx, legacyKeyLayoutAuto)
		vert, _ := st.GetSetting(ctx, legacyKeyLayoutVertical)
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
			_ = st.SetSetting(ctx, keyLayoutMode, mode)
		}
	}
	// The local DB's AllSettings returns every row, legacy ones included,
	// so this needs no extra round trip; a remote map omits the legacy key
	// and the migration happens on the collector instead.
	if all[keyPaneListPct] == "" {
		if all[legacyKeyPaneSplit] == "30/70" {
			out.PaneListPct = 30
			_ = st.SetSetting(ctx, keyPaneListPct, "30")
		}
	}
	return out, nil
}

// Bounds for PaneListPct: either pane keeps at least 10% of the body.
const (
	MinPaneListPct = 10
	MaxPaneListPct = 90
)

// ClampPaneListPct forces pct into [MinPaneListPct, MaxPaneListPct].
func ClampPaneListPct(pct int) int {
	return min(max(pct, MinPaneListPct), MaxPaneListPct)
}

func SetAutoRepoTabs(ctx context.Context, st Store, v bool) error {
	return st.SetSetting(ctx, keyAutoRepoTabs, formatBool(v))
}

func SetBellOnPending(ctx context.Context, st Store, v bool) error {
	return st.SetSetting(ctx, keyBellOnPending, formatBool(v))
}

func SetTailBudgetKB(ctx context.Context, st Store, v int) error {
	return st.SetSetting(ctx, keyTailBudgetKB, strconv.Itoa(v))
}

func SetSummaryTimeoutSec(ctx context.Context, st Store, v int) error {
	return st.SetSetting(ctx, keySummaryTimeoutSec, strconv.Itoa(v))
}

func SetRefreshIntervalMs(ctx context.Context, st Store, v int) error {
	return st.SetSetting(ctx, keyRefreshIntervalMs, strconv.Itoa(v))
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
	// Path marks a KindString value as a directory path, so the settings
	// page offers Tab completion while editing it.
	Path bool
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
	// KindString is free text edited inline on the settings page.
	KindString
)

// Spec.Apply is non-nil only for KindAction rows.
type ActionFunc func(ctx context.Context, st Store, s Settings) (Settings, error)

// AllKeys returns every value-bearing key, derived from AllSpecs (the
// canonical settings table) minus the KindAction rows, which carry no
// stored value. Used by the server's GET /api/settings handler and by
// Local.AllSettings — deriving instead of hand-listing means a new Spec is
// automatically part of the remote settings payload, so a remote TUI can't
// silently fall back to defaults for a key the collector actually has.
func AllKeys() []string {
	specs := AllSpecs()
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		if s.Kind == KindAction {
			continue
		}
		out = append(out, s.Key)
	}
	return out
}

// AllSpecs returns every setting in display order. The TUI uses this both
// to render the modal page and to dispatch updates without a giant switch.
func AllSpecs() []Spec {
	return []Spec{
		{Key: keyAutoRepoTabs, Label: "Auto repo tabs", Help: "Include repo names in the Tab cycle alongside user-named tabs", Kind: KindBool},
		{Key: keyBellOnPending, Label: "Bell on pending", Help: "Ring the terminal bell when the pending count goes from 0 to >0", Kind: KindBool},
		{Key: keyNewestAtBottom, Label: "Newest at bottom", Help: "Show the newest session at the bottom of the list (matches the transcript tail orientation)", Kind: KindBool},
		{Key: keyLayoutMode, Label: "Vertical layout", Help: "Auto = pick from terminal width (vertical when narrow). On = always vertical. Off = always horizontal (side-by-side).", Kind: KindEnum, Options: []string{"auto", "on", "off"}},
		{Key: keyPaneListPct, Label: "Session list size (%)", Help: "Share of the screen the session list takes (width side-by-side, height when vertical); the work pane gets the rest. 30 = compact list, wide work pane. Also adjustable with < / > on the dashboard.", Kind: KindInt, Min: MinPaneListPct, Max: MaxPaneListPct},
		{Key: keyInvertListScroll, Label: "Invert list scroll", Help: "Reverse the mouse wheel over the session list (wheel down moves the selection up). The right pane is unaffected.", Kind: KindBool},
		{Key: keyTimeFormat, Label: "Session time", Help: "How the session list shows each session's last prompt time: relative (4m, 2h) or absolute (09/30 18:45).", Kind: KindEnum, Options: []string{"relative", "absolute"}},
		{Key: keyVerticalAutoCols, Label: "Vertical auto threshold (cols)", Help: "Width in columns below which auto-layout flips to vertical. Lower = stay horizontal longer; higher = go vertical sooner.", Kind: KindInt, Min: 40, Max: 240},
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
		{Key: keyNewSessionDir, Label: "New session directory", Help: "Where the 'n' directory picker starts (~/ allowed). Empty = home directory.", Kind: KindString, Path: true},
		// System section (rendered under its own heading with the version;
		// keep it last).
		{Key: KeyUpdate, Label: "Update ccdash", Help: "Check for a new release, show its notes, install it (y), then offer to restart into it.", Kind: KindAction},
		{Key: KeyRestart, Label: "Restart ccdash", Help: "Stop the collector and relaunch ccdash so a newly installed binary takes effect. Live sessions hosted by ccdash are stopped (asks first).", Kind: KindAction},
	}
}

// applySecurePreset turns off every risk-bearing capability in one shot.
// Convenience for operators who want pure observation without auditing each
// flag individually.
func applySecurePreset(ctx context.Context, st Store, s Settings) (Settings, error) {
	for _, k := range []string{keyApproveEnabled, keySummaryEnabled, keyAttachEnabled, keyAutoInstallSync} {
		next, err := Set(ctx, st, s, k, false)
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
	case keyTailBudgetKB:
		return s.TailBudgetKB
	case keySummaryTimeoutSec:
		return s.SummaryTimeoutSec
	case keyRefreshIntervalMs:
		return s.RefreshIntervalMs
	case keyNewSessionDir:
		return s.NewSessionDir
	case keyPaneListPct:
		return s.PaneListPct
	case keyTimeFormat:
		return s.TimeFormat
	case keyInvertListScroll:
		return s.InvertListScroll
	}
	return nil
}

// Set updates the in-memory snapshot AND persists the change. Returns the
// updated Settings so the caller can replace its copy in one line.
func Set(ctx context.Context, st Store, s Settings, key string, value any) (Settings, error) {
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
	case keyTailBudgetKB:
		s.TailBudgetKB = value.(int)
	case keySummaryTimeoutSec:
		s.SummaryTimeoutSec = value.(int)
	case keyRefreshIntervalMs:
		s.RefreshIntervalMs = value.(int)
	case keyNewSessionDir:
		s.NewSessionDir = value.(string)
	case keyPaneListPct:
		value = ClampPaneListPct(value.(int))
		s.PaneListPct = value.(int)
	case keyTimeFormat:
		s.TimeFormat = value.(string)
	case keyInvertListScroll:
		s.InvertListScroll = value.(bool)
	}
	return s, persist(ctx, st, key, value)
}

func persist(ctx context.Context, st Store, key string, value any) error {
	switch v := value.(type) {
	case bool:
		return st.SetSetting(ctx, key, formatBool(v))
	case int:
		return st.SetSetting(ctx, key, strconv.Itoa(v))
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
		return st.SetSetting(ctx, key, v)
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
