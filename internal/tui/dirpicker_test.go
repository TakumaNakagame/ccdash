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
