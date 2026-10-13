// Package settings provides typed accessors over ccdash's persistent
// preferences (the settings table in SQLite). It centralizes the key
// names, default values, and value parsing in one place so the TUI and
// any future CLI surface read the same source of truth.
package settings

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
	WorkAnim       bool // a light running round working sessions (list rows, grid tiles)

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
	SummaryEnabled  bool // ctrl+t title generation (claude -p spawn); key name predates the summary removal
	AttachEnabled   bool // Enter spawns claude --resume / tmux switch
	AutoInstallSync bool // server boot rewrites settings.json on token mismatch
	HubEnabled      bool // collector dials the hub joined via `ccdash hub join`

	TailBudgetKB      int
	SummaryTimeoutSec int
	RefreshIntervalMs int

	// NewSessionDir is where the `n` directory picker starts. Empty means
	// the home directory. May use ~/.
	NewSessionDir string

	// TrustedDirs lists folders (comma/newline separated, ~/ allowed)
	// whose subfolders the collector marks trusted in ~/.claude.json
	// before starting claude there (internal/claudecfg). Empty = off.
	TrustedDirs string

	// PaneRatio is "list:work:tasks" — how the dashboard's three columns
	// share the width (ParsePaneRatio normalizes it). Without the task
	// pane, list:work share it; in the vertical layout list:work splits
	// the height. Replaces pane_list_pct / pane_tasks_pct (folded in once
	// by Load).
	PaneRatio string
	// TasksPane shows the to-do list as a third column (w). ListCollapsed
	// folds the session list to a strip of numbers (b).
	TasksPane     bool
	ListCollapsed bool
	// TaskSpacing puts a blank line between tasks in the task pane.
	TaskSpacing bool
	// InvertListScroll flips the mouse wheel over the session list: wheel
	// down moves the selection up and vice versa. The right pane keeps
	// its normal direction.
	InvertListScroll bool
	// TimeFormat renders session times as "relative" (4m, 2h, 3d) or
	// "absolute" (09/30 18:45).
	TimeFormat string
	// UpdateChannel is what the update check / `u` / `ccdash update`
	// follow: "stable" (tagged releases) or "dev" (pre-releases too).
	UpdateChannel string
	// AutoArchiveDays archives sessions idle longer than this many days
	// once a day (the collector's first start of the day, or its first
	// discovery tick after midnight). 0 = off.
	AutoArchiveDays int
	// AutoTitle titles a session with claude -p by itself after its first
	// exchange or two (needs SummaryEnabled).
	AutoTitle bool
	// HideHeadless files a new `claude -p` session (a script, a scheduled
	// job) as archived the moment it appears; it comes back when someone
	// resumes it interactively.
	HideHeadless bool
}

const (
	keyAutoRepoTabs   = "auto_repo_tabs"
	keyBellOnPending  = "bell_on_pending"
	keyNewestAtBottom = "newest_at_bottom"
	keyWorkAnim       = "work_anim"
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
	keyHubEnabled           = "hub_enabled"
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
	keyTrustedDirs       = "trusted_dirs"
	keyPaneRatio         = "pane_ratio"
	keyTasksPane         = "tasks_pane"
	keyListCollapsed     = "list_collapsed"
	keyTaskSpacing       = "task_spacing"
	// legacyKeyPaneListPct / legacyKeyPaneTasksPct were the two
	// percentages PaneRatio replaced; Load folds them in once.
	legacyKeyPaneListPct  = "pane_list_pct"
	legacyKeyPaneTasksPct = "pane_tasks_pct"
	// legacyKeyPaneSplit was the "50/50" / "30/70" enum PaneListPct
	// replaced; Load folds it in once.
	legacyKeyPaneSplit  = "pane_split"
	keyTimeFormat       = "time_format"
	keyUpdateChannel    = "update_channel"
	keyInvertListScroll = "invert_list_scroll"
	keyAutoArchiveDays  = "auto_archive_days"
	keyAutoTitle        = "auto_title"
	keyHideHeadless     = "hide_headless"
	// KeyAutoArchiveLast is bookkeeping, not a preference: the local date
	// (YYYY-MM-DD) the daily auto-archive last ran. Not in AllSpecs.
	KeyAutoArchiveLast = "auto_archive_last"
)

// Defaults returns the baseline values used whenever a key is missing.
func Defaults() Settings {
	return Settings{
		AutoRepoTabs:     true,
		BellOnPending:    true,
		NewestAtBottom:   false,
		WorkAnim:         true,
		LayoutMode:       "auto",
		VerticalAutoCols: 100,
		ApproveEnabled:   true,
		SummaryEnabled:   true,
		AttachEnabled:    true,
		AutoInstallSync:  true,
		HubEnabled:       true,

		TailBudgetKB:      256,
		SummaryTimeoutSec: 180,
		RefreshIntervalMs: 1000,

		PaneRatio:     "36:36:28",
		TasksPane:     true,
		TimeFormat:    "relative",
		UpdateChannel: "stable",

		AutoArchiveDays: 7,
		AutoTitle:       true,
		HideHeadless:    true,
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
		{keyWorkAnim, func(v string) { out.WorkAnim = parseBool(v, out.WorkAnim) }},
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
		{keyHubEnabled, func(v string) { out.HubEnabled = parseBool(v, out.HubEnabled) }},
		{keyTailBudgetKB, func(v string) { out.TailBudgetKB = parseInt(v, out.TailBudgetKB) }},
		{keySummaryTimeoutSec, func(v string) { out.SummaryTimeoutSec = parseInt(v, out.SummaryTimeoutSec) }},
		{keyRefreshIntervalMs, func(v string) { out.RefreshIntervalMs = parseInt(v, out.RefreshIntervalMs) }},
		{keyNewSessionDir, func(v string) { out.NewSessionDir = v }},
		{keyTrustedDirs, func(v string) { out.TrustedDirs = v }},
		{keyPaneRatio, func(v string) {
			if r, err := ParsePaneRatio(v); err == nil {
				out.PaneRatio = r.String()
			}
		}},
		{keyInvertListScroll, func(v string) { out.InvertListScroll = parseBool(v, out.InvertListScroll) }},
		{keyTasksPane, func(v string) { out.TasksPane = parseBool(v, out.TasksPane) }},
		{keyListCollapsed, func(v string) { out.ListCollapsed = parseBool(v, out.ListCollapsed) }},
		{keyTaskSpacing, func(v string) { out.TaskSpacing = parseBool(v, out.TaskSpacing) }},
		{keyAutoArchiveDays, func(v string) { out.AutoArchiveDays = parseInt(v, out.AutoArchiveDays) }},
		{keyAutoTitle, func(v string) { out.AutoTitle = parseBool(v, out.AutoTitle) }},
		{keyHideHeadless, func(v string) { out.HideHeadless = parseBool(v, out.HideHeadless) }},
		{keyTimeFormat, func(v string) {
			if v == "relative" || v == "absolute" {
				out.TimeFormat = v
			}
		}},
		{keyUpdateChannel, func(v string) {
			if v == "stable" || v == "dev" {
				out.UpdateChannel = v
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
	if all[keyPaneRatio] == "" {
		if r, ok := legacyPaneRatio(all); ok {
			out.PaneRatio = r.String()
			_ = st.SetSetting(ctx, keyPaneRatio, out.PaneRatio)
		}
	}
	return out, nil
}

// legacyPaneRatio turns the old settings into a ratio that draws the same
// screen: pane_list_pct was the list's share of list + work, pane_tasks_pct
// the task pane's share of the width (and the even older pane_split enum
// meant 30 or 50).
func legacyPaneRatio(all map[string]string) (PaneRatio, bool) {
	list, tasks := -1, -1
	if v := all[legacyKeyPaneListPct]; v != "" {
		list = parseInt(v, -1)
	} else if all[legacyKeyPaneSplit] == "30/70" {
		list = 30
	} else if all[legacyKeyPaneSplit] == "50/50" {
		list = 50
	}
	if v := all[legacyKeyPaneTasksPct]; v != "" {
		tasks = parseInt(v, -1)
	}
	if list < 0 && tasks < 0 {
		return PaneRatio{}, false
	}
	if list < 0 {
		list = 50
	}
	if tasks < 0 {
		tasks = 28
	}
	list = min(max(list, 10), 90)
	tasks = min(max(tasks, 10), 60)
	l := list * (100 - tasks) / 100
	return PaneRatio{List: l, Work: 100 - tasks - l, Tasks: tasks}, true
}

// PaneRatio is the three column shares, in percent of their sum.
type PaneRatio struct{ List, Work, Tasks int }

// Pane share bounds: each column keeps at least MinPaneShare.
const MinPaneShare = 10

// ParsePaneRatio reads "list:work:tasks" (any positive numbers; ":" or
// "/" or spaces between them) and scales it to sum to 100, each part at
// least MinPaneShare.
func ParsePaneRatio(v string) (PaneRatio, error) {
	f := strings.FieldsFunc(v, func(r rune) bool { return r == ':' || r == '/' || r == ' ' || r == ',' })
	if len(f) != 3 {
		return PaneRatio{}, fmt.Errorf("pane ratio is list:work:tasks, e.g. 25:50:25")
	}
	var n [3]int
	sum := 0
	for i, x := range f {
		v, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil || v <= 0 {
			return PaneRatio{}, fmt.Errorf("pane ratio is three positive numbers, e.g. 25:50:25")
		}
		n[i] = v
		sum += v
	}
	for i := range n {
		n[i] = max(n[i]*100/sum, MinPaneShare)
	}
	// Give the rounding remainder (or take the excess) from the work pane.
	n[1] += 100 - n[0] - n[1] - n[2]
	if n[1] < MinPaneShare {
		return PaneRatio{}, fmt.Errorf("pane ratio leaves no room for the work pane")
	}
	return PaneRatio{List: n[0], Work: n[1], Tasks: n[2]}, nil
}

// Ratio parses s.PaneRatio (falling back to the default).
func (s Settings) Ratio() PaneRatio {
	if r, err := ParsePaneRatio(s.PaneRatio); err == nil {
		return r
	}
	r, _ := ParsePaneRatio(Defaults().PaneRatio)
	return r
}

func (r PaneRatio) String() string { return fmt.Sprintf("%d:%d:%d", r.List, r.Work, r.Tasks) }

// ListShare is the list's percent of list + work (the width without the
// task pane, the height in the vertical layout).
func (r PaneRatio) ListShare() int { return r.List * 100 / max(1, r.List+r.Work) }

// MoveList moves step percent from the work pane to the list (negative:
// back), keeping both above MinPaneShare.
func (r PaneRatio) MoveList(step int) PaneRatio {
	step = min(max(step, MinPaneShare-r.List), r.Work-MinPaneShare)
	r.List += step
	r.Work -= step
	return r
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
		{Key: keyWorkAnim, Label: "Working animation", Help: "Run a light round the edge of sessions that are working — list rows and grid tiles (the spinner stays either way)", Kind: KindBool},
		{Key: keyLayoutMode, Label: "Vertical layout", Help: "Auto = pick from terminal width (vertical when narrow). On = always vertical. Off = always horizontal (side-by-side).", Kind: KindEnum, Options: []string{"auto", "on", "off"}},
		{Key: keyPaneRatio, Label: "Pane widths (list:work:tasks)", Help: "How the session list, the work pane and the task pane share the width, e.g. 25:50:25 (any numbers; each keeps at least 10%). Without the task pane the list and work pane share it in the same proportion; in the vertical layout that proportion splits the height. < / > on the dashboard move 5% between the list and the work pane.", Kind: KindString},
		{Key: keyTasksPane, Label: "Task pane", Help: "Show the to-do list as a column on the right of the dashboard and the grid (also w).", Kind: KindBool},
		{Key: keyTaskSpacing, Label: "Space between tasks", Help: "Put a blank line between the tasks in the task pane (each task is two lines; every other one has a faint band either way).", Kind: KindBool},
		{Key: keyListCollapsed, Label: "Collapse session list", Help: "Fold the session list to a strip of session numbers (also b); j/k still move the selection.", Kind: KindBool},
		{Key: keyInvertListScroll, Label: "Invert list scroll", Help: "Reverse the mouse wheel over the session list (wheel down moves the selection up). The right pane is unaffected.", Kind: KindBool},
		{Key: keyTimeFormat, Label: "Session time", Help: "How the session list shows each session's last prompt time: relative (4m, 2h) or absolute (09/30 18:45).", Kind: KindEnum, Options: []string{"relative", "absolute"}},
		{Key: keyUpdateChannel, Label: "Update channel", Help: "What the update check, u and `ccdash update` follow: stable = tagged releases only, dev = pre-releases (rc) too.", Kind: KindEnum, Options: []string{"stable", "dev"}},
		{Key: keyVerticalAutoCols, Label: "Vertical auto threshold (cols)", Help: "Width in columns below which auto-layout flips to vertical. Lower = stay horizontal longer; higher = go vertical sooner.", Kind: KindInt, Min: 40, Max: 240},
		// Risk-bearing toggles
		{Key: keyApproveEnabled, Label: "Approval blocking", Help: "When OFF, ccdash never holds PermissionRequest hooks — Claude prompts you in the terminal as it would without ccdash, and the a/A/d shortcuts are disabled", Kind: KindBool},
		{Key: keySummaryEnabled, Label: "Title generation via claude -p", Help: "When OFF, ctrl+t (generate titles) is disabled and ccdash never spawns claude -p (no transcript digests sent over the network)", Kind: KindBool},
		{Key: keyAutoTitle, Label: "Auto-generate titles", Help: "Title each session with claude -p by itself after its first exchange or two (needs title generation ON). ctrl+t still re-generates on demand.", Kind: KindBool},
		{Key: keyHideHeadless, Label: "Hide headless sessions", Help: "File each new claude -p session (scripts, scheduled jobs) as archived right away. Resuming one interactively brings it back.", Kind: KindBool},
		{Key: keyAttachEnabled, Label: "Attach (enter)", Help: "When OFF, Enter only shows session info — ccdash never spawns claude --resume or runs tmux switch-client", Kind: KindBool},
		{Key: keyAutoInstallSync, Label: "Auto-rewrite settings.json", Help: "When OFF, server start does NOT silently rewrite ~/.claude/settings.json when the token rotates; you'll need to run install-hooks manually", Kind: KindBool},
		{Key: keyHubEnabled, Label: "Hub connection", Help: "When OFF, the collector does not connect to the hub joined via `ccdash hub join`, so this machine is unreachable from the web portal", Kind: KindBool},
		{Key: keyPresetSecure, Label: "Apply secure preset", Help: "Observation-only mode: turns off approval blocking, title generation, attach, auto-install sync, and the hub connection in one go", Kind: KindAction, Apply: applySecurePreset},
		// Numeric tunables
		{Key: keyTailBudgetKB, Label: "Right-pane tail budget (KB)", Help: "Bytes of transcript loaded for the inline live tail; bigger == more context, slower", Kind: KindInt, Min: 32, Max: 8192},
		{Key: keySummaryTimeoutSec, Label: "Title generation timeout (s)", Help: "How long to wait for `claude -p` to produce titles before giving up", Kind: KindInt, Min: 30, Max: 600},
		{Key: keyAutoArchiveDays, Label: "Auto-archive after (days)", Help: "Once a day (first collector start of the day), archive sessions idle longer than this. Favorites, project members and running sessions are kept; resuming an auto-archived session brings it back. 0 = off.", Kind: KindInt, Min: 0, Max: 365},
		{Key: keyRefreshIntervalMs, Label: "Refresh interval (ms)", Help: "How often the TUI re-queries the DB for new state", Kind: KindInt, Min: 250, Max: 10000},
		{Key: keyNewSessionDir, Label: "New session directory", Help: "Where the 'n' directory picker starts (~/ allowed). Empty = home directory.", Kind: KindString, Path: true},
		{Key: keyTrustedDirs, Label: "Trusted folders", Help: "Folders (comma-separated, ~/ allowed) where claude started by ccdash (n, the portal, MCP start_session) skips Claude Code's folder-trust question: ccdash marks the folder trusted in ~/.claude.json first. Covers subfolders. Empty = off.", Kind: KindString},
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
	for _, k := range []string{keyApproveEnabled, keySummaryEnabled, keyAttachEnabled, keyAutoInstallSync, keyHubEnabled} {
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
	case keyWorkAnim:
		return s.WorkAnim
	case keyTasksPane:
		return s.TasksPane
	case keyListCollapsed:
		return s.ListCollapsed
	case keyTaskSpacing:
		return s.TaskSpacing
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
	case keyHubEnabled:
		return s.HubEnabled
	case keyTailBudgetKB:
		return s.TailBudgetKB
	case keySummaryTimeoutSec:
		return s.SummaryTimeoutSec
	case keyRefreshIntervalMs:
		return s.RefreshIntervalMs
	case keyNewSessionDir:
		return s.NewSessionDir
	case keyTrustedDirs:
		return s.TrustedDirs
	case keyPaneRatio:
		return s.PaneRatio
	case keyTimeFormat:
		return s.TimeFormat
	case keyUpdateChannel:
		return s.UpdateChannel
	case keyInvertListScroll:
		return s.InvertListScroll
	case keyAutoArchiveDays:
		return s.AutoArchiveDays
	case keyAutoTitle:
		return s.AutoTitle
	case keyHideHeadless:
		return s.HideHeadless
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
	case keyWorkAnim:
		s.WorkAnim = value.(bool)
	case keyTasksPane:
		s.TasksPane = value.(bool)
	case keyListCollapsed:
		s.ListCollapsed = value.(bool)
	case keyTaskSpacing:
		s.TaskSpacing = value.(bool)
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
	case keyHubEnabled:
		s.HubEnabled = value.(bool)
	case keyTailBudgetKB:
		s.TailBudgetKB = value.(int)
	case keySummaryTimeoutSec:
		s.SummaryTimeoutSec = value.(int)
	case keyRefreshIntervalMs:
		s.RefreshIntervalMs = value.(int)
	case keyNewSessionDir:
		s.NewSessionDir = value.(string)
	case keyTrustedDirs:
		s.TrustedDirs = value.(string)
	case keyPaneRatio:
		r, err := ParsePaneRatio(value.(string))
		if err != nil {
			return s, err
		}
		value = r.String()
		s.PaneRatio = r.String()
	case keyTimeFormat:
		s.TimeFormat = value.(string)
	case keyUpdateChannel:
		s.UpdateChannel = value.(string)
	case keyInvertListScroll:
		s.InvertListScroll = value.(bool)
	case keyAutoArchiveDays:
		s.AutoArchiveDays = value.(int)
	case keyAutoTitle:
		s.AutoTitle = value.(bool)
	case keyHideHeadless:
		s.HideHeadless = value.(bool)
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
