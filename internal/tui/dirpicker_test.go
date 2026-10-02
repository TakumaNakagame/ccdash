package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/settings"
	"github.com/takumanakagame/ccmanage/internal/store"
)

func pickerModel(t *testing.T) (*model, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{"work/alpha", "work/beta", "work/Alphabet", "work/xalpha", "work/.hidden", "other"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 120, 40
	m.settings.AttachEnabled = true
	return m, home
}

func labels(cs []dirCand) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.label)
	}
	return out
}

func TestDirPickerCandidates(t *testing.T) {
	m, home := pickerModel(t)
	m.settings.NewSessionDir = "~/work"
	m.allSessions = []mdl.Session{
		{SessionID: "a", Cwd: filepath.Join(home, "other"), LastSeen: time.Now()},
	}
	m.openDirPicker()
	if m.newSessionBuffer != "~/work/" {
		t.Fatalf("start = %q, want configured ~/work/", m.newSessionBuffer)
	}
	got := strings.Join(labels(m.dirCandidates()), ",")
	if got != "../,Alphabet/,alpha/,beta/,xalpha/" {
		t.Fatalf("listing = %s", got)
	}

	// Partial segment: prefix matches (case-insensitive) before substring.
	m.newSessionBuffer = "~/work/alp"
	got = strings.Join(labels(m.dirCandidates()), ",")
	if got != "Alphabet/,alpha/,xalpha/" {
		t.Fatalf("filtered = %s", got)
	}

	// Recent cwds show up when they match the input.
	m.newSessionBuffer = "~/oth"
	cs := m.dirCandidates()
	var recent []string
	for _, c := range cs {
		if c.recent {
			recent = append(recent, c.label)
		}
	}
	if len(recent) != 0 {
		// ~/other is already listed from disk; no duplicate recent row.
		t.Fatalf("duplicate recent rows: %v (all %v)", recent, labels(cs))
	}

	// A recent cwd deeper than the listing is offered as a shortcut.
	m.allSessions = append(m.allSessions, mdl.Session{SessionID: "b", Cwd: filepath.Join(home, "work", "alpha"), LastSeen: time.Now()})
	m.newSessionBuffer = "~/"
	recent = nil
	for _, c := range m.dirCandidates() {
		if c.recent {
			recent = append(recent, c.path)
		}
	}
	if strings.Join(recent, ",") != "~/work/alpha/" {
		t.Fatalf("recent = %v", recent)
	}
}

func TestDirPickerKeys(t *testing.T) {
	m, _ := pickerModel(t)
	m.settings.NewSessionDir = "~/work"
	m.openDirPicker()
	press := func(k tea.KeyPressMsg) { m.handleKeyNewSessionEdit(k) }

	press(tea.KeyPressMsg{Code: tea.KeyDown}) // ../
	press(tea.KeyPressMsg{Code: tea.KeyDown}) // Alphabet/
	press(tea.KeyPressMsg{Code: tea.KeyDown}) // alpha/
	press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.newSessionBuffer != "~/work/alpha/" || m.pickSel != -1 || !m.editingNewSession {
		t.Fatalf("after pick: buf=%q sel=%d open=%v", m.newSessionBuffer, m.pickSel, m.editingNewSession)
	}
	press(tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.newSessionBuffer != "~/work/" {
		t.Fatalf("after left: %q", m.newSessionBuffer)
	}
	press(tea.KeyPressMsg{Text: "b", Code: 'b'})
	press(tea.KeyPressMsg{Code: tea.KeyTab}) // highlight first match
	press(tea.KeyPressMsg{Code: tea.KeyTab}) // substitute it
	if m.newSessionBuffer != "~/work/beta/" {
		t.Fatalf("typed+tab: %q", m.newSessionBuffer)
	}
	press(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.editingNewSession {
		t.Fatal("esc did not close the picker")
	}
}

func TestDirPickerOverlayKeepsWidth(t *testing.T) {
	m, _ := pickerModel(t)
	base := strings.Split(m.View().Content, "\n")
	m.openDirPicker()
	v := m.View()
	rows := strings.Split(v.Content, "\n")
	if len(rows) != len(base) {
		t.Fatalf("overlay changed the row count: %d -> %d", len(base), len(rows))
	}
	for i, row := range rows {
		if w, bw := ansi.StringWidth(row), ansi.StringWidth(base[i]); w > max(bw, m.width) {
			t.Fatalf("row %d grew to %d cols (base %d)", i, w, bw)
		}
	}
	if !strings.Contains(ansi.Strip(v.Content), "new claude session") {
		t.Fatal("picker window not drawn")
	}
	if v.Cursor == nil {
		t.Fatal("no caret for the path input")
	}
}

func TestCompletePath(t *testing.T) {
	_, _ = pickerModel(t) // sets HOME and the tree
	cases := []struct{ in, want string }{
		{"~/wo", "~/work/"},            // single match → filled + slash
		{"~/work/b", "~/work/beta/"},   // single
		{"~/work/al", "~/work/alpha/"}, // alpha only; Alphabet differs in case, xalpha isn't a prefix match
		{"~/work/zz", "~/work/zz"},     // no match → unchanged
	}
	for _, c := range cases {
		if got, _ := completePath(c.in); got != c.want {
			t.Errorf("completePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Several matches: extend to the common prefix and report the names.
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), "work", "betamax"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, names := completePath("~/work/b")
	if got != "~/work/beta" || len(names) != 2 {
		t.Fatalf("got %q names %v, want ~/work/beta with 2 names", got, names)
	}
}

func TestSettingsHelpShownOnce(t *testing.T) {
	m, _ := pickerModel(t)
	m.pane = paneSettings
	out := ansi.Strip(m.View().Content)
	if n := strings.Count(out, "esc back"); n != 1 {
		t.Fatalf("settings help appears %d times, want 1", n)
	}
	// Editing the path setting advertises Tab completion.
	for i, s := range settings.AllSpecs() {
		if s.Path {
			m.settingsSel = i
		}
	}
	m.settingsEdit = true
	if out := ansi.Strip(m.View().Content); !strings.Contains(out, "tab complete") {
		t.Fatal("path edit footer lacks the tab hint")
	}
}

// TestPathSettingSuggestions: entering edit on the path setting shows the
// candidates at once, right under the input and only as wide as needed;
// ↓ + Enter substitutes, Enter again saves.
func TestPathSettingSuggestions(t *testing.T) {
	m, _ := pickerModel(t)
	m.store = &memStore{kv: map[string]string{}}
	m.pane = paneSettings
	for i, s := range settings.AllSpecs() {
		if s.Path {
			m.settingsSel = i
		}
	}
	m.handleKeySettings(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.settingsEdit || m.settingsBuffer != "~/" {
		t.Fatalf("edit=%v buf=%q, want editing from ~/", m.settingsEdit, m.settingsBuffer)
	}
	out := ansi.Strip(m.renderSettingsBody(200))
	rows := strings.Split(out, "\n")
	inputRow := -1
	for i, r := range rows {
		if strings.Contains(r, "New session directory") {
			inputRow = i
		}
	}
	if inputRow < 0 || !strings.Contains(rows[inputRow+2], "other/") && !strings.Contains(rows[inputRow+2], "work/") {
		t.Fatalf("no suggestions right under the input:\n%s", strings.Join(rows[max(0, inputRow):min(len(rows), inputRow+6)], "\n"))
	}
	if w := ansi.StringWidth(strings.TrimRight(rows[inputRow+1], " ")); w >= m.width {
		t.Fatalf("suggestion box spans the full width (%d cols)", w)
	}

	m.handleKeySettings(tea.KeyPressMsg{Text: "w", Code: 'w'})
	m.handleKeySettings(tea.KeyPressMsg{Code: tea.KeyDown})
	m.handleKeySettings(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.settingsBuffer != "~/work/" || !m.settingsEdit {
		t.Fatalf("after pick: buf=%q edit=%v", m.settingsBuffer, m.settingsEdit)
	}
	m.handleKeySettings(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.settingsEdit || m.settings.NewSessionDir != "~/work/" {
		t.Fatalf("after save: edit=%v dir=%q", m.settingsEdit, m.settings.NewSessionDir)
	}
}

// memStore implements just the settings methods of store.Store; any
// other call panics on the nil embedded interface.
type memStore struct {
	store.Store
	kv map[string]string
}

func (s *memStore) GetSetting(_ context.Context, k string) (string, error) { return s.kv[k], nil }
func (s *memStore) SetSetting(_ context.Context, k, v string) error        { s.kv[k] = v; return nil }
func (s *memStore) AllSettings(context.Context) (map[string]string, error) { return s.kv, nil }

func TestPaneSplitCompact(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 200, 40
	m.settings.LayoutMode = "horizontal"
	m.sessions = []mdl.Session{{SessionID: "s1"}} // rightPaneGeom needs a selection
	if got := m.leftPaneWidth(); got != 100 {
		t.Fatalf("50/50: left = %d", got)
	}
	m.settings.PaneListPct = 30
	if got := m.leftPaneWidth(); got != 60 {
		t.Fatalf("30/70: left = %d", got)
	}
	// Live pane geometry and mouse zoning follow the same split.
	g, ok := m.rightPaneGeom()
	if !ok || g.x != 63 {
		t.Fatalf("right pane x = %d (ok=%v), want 63", g.x, ok)
	}
	if m.mouseInRightPane(tea.Mouse{X: 62}) || !m.mouseInRightPane(tea.Mouse{X: 63}) {
		t.Fatal("mouse zoning disagrees with the split")
	}
	// Narrow terminals keep the 30-col minimum.
	m.width = 80
	if got := m.leftPaneWidth(); got != 30 {
		t.Fatalf("narrow: left = %d", got)
	}
}

// TestPaneListPctBounds: whatever the stored value, neither pane drops
// below 10% (or the 30-col list / 20-col right / 5-line minimums).
func TestPaneListPctBounds(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.store = &memStore{kv: map[string]string{}}
	m.width, m.height = 200, 40

	for _, pct := range []int{-50, 0, 5, 95, 100, 1000} {
		m.settings.PaneListPct = pct
		left := m.leftPaneWidth()
		if right := m.width - left - 3; left < 30 || right < 20 || left < m.width/10 || right < m.width/10-3 {
			t.Errorf("pct %d: left %d right %d", pct, left, right)
		}
		listH, rightH := m.verticalSplit(30)
		if listH < 5 || rightH < 5 || listH+rightH+1 != 30 {
			t.Errorf("pct %d: vertical %d/%d", pct, listH, rightH)
		}
	}

	// < / > step by 5 and stop at the bounds, persisting each change.
	m.settings.PaneListPct = 50
	for range 20 {
		m.adjustPaneSplit(-5)
	}
	if m.settings.PaneListPct != settings.MinPaneListPct {
		t.Fatalf("shrunk to %d, want %d", m.settings.PaneListPct, settings.MinPaneListPct)
	}
	m.adjustPaneSplit(5)
	if got := m.store.(*memStore).kv["pane_list_pct"]; got != "15" {
		t.Fatalf("persisted %q, want 15", got)
	}
	for range 20 {
		m.adjustPaneSplit(5)
	}
	if m.settings.PaneListPct != settings.MaxPaneListPct {
		t.Fatalf("grew to %d, want %d", m.settings.PaneListPct, settings.MaxPaneListPct)
	}
}

func TestSessionRowAbsoluteTime(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	ts := time.Date(2026, 9, 30, 18, 45, 0, 0, time.Local)
	s := mdl.Session{SessionID: "abcdef12", Title: "hello", Cwd: "/w/proj", LastSeen: ts}

	m.settings.TimeFormat = "absolute"
	rows := strings.Split(ansi.Strip(m.renderSessionRow(s, false, 60)), "\n")
	if !strings.Contains(rows[0], "09/30 18:45 hello") {
		t.Fatalf("line 1 = %q", rows[0])
	}
	// Line 2 starts under the title.
	title := ansi.StringWidth(rows[0][:strings.Index(rows[0], "hello")])
	if meta := len(rows[1]) - len(strings.TrimLeft(rows[1], " ")); title != meta {
		t.Fatalf("title at col %d, meta at col %d", title, meta)
	}

	m.settings.TimeFormat = "relative"
	rows = strings.Split(ansi.Strip(m.renderSessionRow(s, false, 60)), "\n")
	if strings.Contains(rows[0], "09/30") {
		t.Fatalf("relative mode shows an absolute time: %q", rows[0])
	}
}
