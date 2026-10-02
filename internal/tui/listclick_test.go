package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/settings"
)

// clickRow returns the terminal position of session idx in the rendered
// list (its first line).
func clickRow(t *testing.T, m *model, idx int) tea.Mouse {
	t.Helper()
	m.View() // renders the list, filling listLineSess
	top, _ := m.bodyLayout()
	for i, s := range m.listLineSess {
		if s == idx {
			return tea.Mouse{X: 2, Y: top + i, Button: tea.MouseLeft}
		}
	}
	t.Fatalf("session %d not on screen", idx)
	return tea.Mouse{}
}

func TestListClickSelectsAndDoubleClickResumes(t *testing.T) {
	now := time.Now()
	for _, layout := range []string{"horizontal", "vertical"} {
		t.Run(layout, func(t *testing.T) {
			m := newModel(context.Background(), nil, RemoteInfo{})
			m.width, m.height = 160, 40
			m.settings.LayoutMode = layout
			m.settings.AttachEnabled = true
			m.sessions = []mdl.Session{
				{SessionID: "a", Title: "first", LastSeen: now, Status: mdl.StatusStopped},
				{SessionID: "b", Title: "running elsewhere", LastSeen: now, Status: mdl.StatusActive},
				{SessionID: "c", Title: "stopped", LastSeen: now, Status: mdl.StatusStopped},
			}
			m.allSessions = m.sessions
			click := func(idx int) tea.Cmd {
				_, cmd := m.Update(tea.MouseClickMsg(clickRow(t, m, idx)))
				return cmd
			}

			// Single click selects (preview) without resuming.
			click(2)
			if m.selSess != 2 || m.flash != "" {
				t.Fatalf("single click: sel=%d flash=%q", m.selSess, m.flash)
			}
			// Double click on a stopped session resumes it.
			if cmd := click(2); cmd == nil || m.flash == "" {
				t.Fatalf("double click on stopped: cmd=%v flash=%q", cmd, m.flash)
			}

			// Two clicks too far apart are two single clicks.
			m.flash = ""
			click(0)
			m.lastClickAt = time.Now().Add(-time.Second)
			if click(0); m.flash != "" {
				t.Fatalf("slow second click acted: %q", m.flash)
			}

			// A session running in another terminal asks first.
			click(1)
			if cmd := click(1); cmd != nil || !m.dupConfirm || m.dupSessionID != "b" {
				t.Fatalf("double click on running: cmd=%v confirm=%v id=%q", cmd, m.dupConfirm, m.dupSessionID)
			}
			m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
		})
	}
}

func TestListClickIgnoredUnderModal(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 160, 40
	m.settings.LayoutMode = "horizontal"
	m.sessions = []mdl.Session{{SessionID: "a", LastSeen: time.Now()}, {SessionID: "b", LastSeen: time.Now()}}
	pos := clickRow(t, m, 1)
	m.editingSkill = true
	m.Update(tea.MouseClickMsg(pos))
	if m.selSess != 0 {
		t.Fatal("click went through the skill picker")
	}
	m.editingSkill = false
	m.Update(tea.MouseClickMsg(pos))
	if m.selSess != 1 {
		t.Fatalf("sel=%d, want 1", m.selSess)
	}
	// Clicking in the right pane doesn't change the selection.
	m.Update(tea.MouseClickMsg(tea.Mouse{X: m.leftPaneWidth() + 10, Y: pos.Y, Button: tea.MouseLeft}))
	if m.selSess != 1 {
		t.Fatalf("right-pane click moved selection to %d", m.selSess)
	}
}

// TestInvertListScroll: the setting flips the wheel over the list only.
func TestInvertListScroll(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 160, 40
	m.settings.LayoutMode = "horizontal"
	m.sessions = []mdl.Session{{SessionID: "a"}, {SessionID: "b"}, {SessionID: "c"}}
	m.selSess = 1
	wheel := func(b tea.MouseButton, x int) {
		m.Update(tea.MouseWheelMsg(tea.Mouse{X: x, Y: 10, Button: b}))
	}

	wheel(tea.MouseWheelDown, 2)
	if m.selSess != 2 {
		t.Fatalf("normal: wheel down → sel %d, want 2", m.selSess)
	}
	m.settings.InvertListScroll = true
	wheel(tea.MouseWheelDown, 2)
	if m.selSess != 1 {
		t.Fatalf("inverted: wheel down → sel %d, want 1", m.selSess)
	}
	wheel(tea.MouseWheelUp, 2)
	if m.selSess != 2 {
		t.Fatalf("inverted: wheel up → sel %d, want 2", m.selSess)
	}
	// The right pane keeps its direction: wheel up scrolls back in history.
	wheel(tea.MouseWheelUp, m.leftPaneWidth()+10)
	if m.tailScroll != 3 || m.selSess != 2 {
		t.Fatalf("right pane: tailScroll=%d sel=%d", m.tailScroll, m.selSess)
	}
}

// TestWrapOnlyOnFreshPress: a held key / fast run stops at the end of the
// list; a press after a pause wraps.
func TestWrapOnlyOnFreshPress(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.sessions = []mdl.Session{{SessionID: "a"}, {SessionID: "b"}, {SessionID: "c"}}
	down := func(repeat bool) { m.Update(tea.KeyPressMsg{Code: 'j', Text: "j", IsRepeat: repeat}) }
	pause := func() { m.lastNavAt = time.Now().Add(-time.Second) }

	down(false)
	down(true)
	if m.selSess != 2 {
		t.Fatalf("sel=%d, want 2", m.selSess)
	}
	down(true) // held key reaches the end: stop
	if m.selSess != 2 {
		t.Fatalf("held key wrapped to %d", m.selSess)
	}
	down(false) // fast re-tap right after (no repeat flag): still stop
	if m.selSess != 2 {
		t.Fatalf("fast tap wrapped to %d", m.selSess)
	}
	pause()
	down(false) // fresh press after a pause: wrap
	if m.selSess != 0 {
		t.Fatalf("fresh press: sel=%d, want 0 (wrapped)", m.selSess)
	}
	pause()
	m.Update(tea.KeyPressMsg{Code: 'k', Text: "k"}) // and the other way
	if m.selSess != 2 {
		t.Fatalf("fresh k at top: sel=%d, want 2", m.selSess)
	}
}

// TestSettingsScrollKeepsWindow: after reaching the bottom of a page that
// doesn't fit, moving up moves the cursor within the same window and only
// scrolls once the cursor reaches the top edge.
func TestSettingsScrollKeepsWindow(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 120, 20
	m.pane = paneSettings
	const h = 14
	render := func() string { return m.renderSettingsBody(h) }
	n := len(settings.AllSpecs())

	for m.settingsSel = 0; m.settingsSel < n; m.settingsSel++ {
		render()
	}
	m.settingsSel = n - 1
	render()
	bottom := m.settingsScroll
	if bottom == 0 {
		t.Fatal("page fits; test needs a smaller height")
	}

	m.settingsSel = n - 2
	render()
	if m.settingsScroll != bottom {
		t.Fatalf("one step up scrolled %d → %d", bottom, m.settingsScroll)
	}
	// Keep going up: the window holds until the cursor would leave it,
	// then the selected row sits at the top edge.
	for m.settingsSel > 0 {
		m.settingsSel--
		prev := m.settingsScroll
		out := render()
		if m.settingsScroll != prev && m.settingsSel > 0 { // row 0 also reveals the page header
			first := strings.SplitN(ansi.Strip(out), "\n", 2)[0]
			if !strings.Contains(first, settings.AllSpecs()[m.settingsSel].Label) {
				t.Fatalf("scrolled but selected row not at top: %q", first)
			}
		}
	}
	if m.settingsScroll != 0 {
		t.Fatalf("at first row scroll=%d, want 0", m.settingsScroll)
	}
}

// TestDupConfirm: enter on a session running in another terminal opens the
// confirmation; enter / n / esc there cancel, only y resumes it here.
func TestDupConfirm(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 120, 40
	m.settings.AttachEnabled = true
	m.sessions = []mdl.Session{{SessionID: "run1", Title: "busy elsewhere", Cwd: "/w/x", Status: mdl.StatusActive, ProcPID: 2873}}
	enter := func() tea.Cmd { _, c := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); return c }

	if cmd := enter(); cmd != nil || !m.dupConfirm {
		t.Fatalf("enter: cmd=%v confirm=%v", cmd, m.dupConfirm)
	}
	db, _ := m.dupConfirmBox()
	box := ansi.Strip(db)
	for _, want := range []string{"already running", "busy elsewhere", "pid 2873", "second claude", "Yes, open a second one"} {
		if !strings.Contains(box, want) {
			t.Errorf("modal missing %q:\n%s", want, box)
		}
	}
	if out := ansi.Strip(m.View().Content); !strings.Contains(out, "already running") {
		t.Fatal("modal not drawn")
	}
	// enter inside the modal is "no": the risky choice needs y.
	if cmd := enter(); cmd != nil || m.dupConfirm {
		t.Fatalf("enter in modal: cmd=%v confirm=%v", cmd, m.dupConfirm)
	}
	enter()
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.dupConfirm {
		t.Fatal("esc did not close the modal")
	}
	enter()
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"}); cmd == nil || m.dupConfirm {
		t.Fatalf("y: cmd=%v confirm=%v", cmd, m.dupConfirm)
	}

	// Stopped sessions and tmux panes don't ask.
	m.sessions[0].Status = mdl.StatusStopped
	if enter(); m.dupConfirm {
		t.Fatal("stopped session asked")
	}
	m.sessions[0].Status, m.sessions[0].Pane = mdl.StatusActive, "%3"
	if enter(); m.dupConfirm {
		t.Fatal("tmux session asked")
	}
}
