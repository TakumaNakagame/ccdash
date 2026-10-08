package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/takumanakagame/ccmanage/internal/buildinfo"
	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/selfupdate"
	"github.com/takumanakagame/ccmanage/internal/settings"
)

// TestRestartConfirm: the settings action opens a warning window naming
// the live sessions it would stop; only y / enter actually restarts.
func TestRestartConfirm(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 120, 40
	m.allSessions = []mdl.Session{
		{SessionID: "live1", Title: "fix the login bug"},
		{SessionID: "idle1", Title: "not hosted"},
	}
	m.ptyAlive = map[string]bool{"live1": true}
	m.approvals = []mdl.Approval{{Status: mdl.ApprovalPending}}

	m.pane = paneSettings
	for i, s := range settings.AllSpecs() {
		if s.Key == settings.KeyRestart {
			m.settingsSel = i
		}
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.restartConfirm {
		t.Fatal("restart row did not open the confirmation")
	}
	rb, _ := m.restartBox()
	box := ansi.Strip(rb)
	for _, want := range []string{"1 live session(s)", "(claude --resume)", "fix the login bug", "1 pending approval", "resume"} {
		if !strings.Contains(box, want) {
			t.Errorf("modal missing %q:\n%s", want, box)
		}
	}
	if strings.Contains(box, "not hosted") {
		t.Errorf("modal lists a session ccdash doesn't host:\n%s", box)
	}
	if out := ansi.Strip(m.View().Content); !strings.Contains(out, "restart ccdash?") {
		t.Fatal("modal not drawn over the view")
	}

	m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if m.restartConfirm || m.restartRequested {
		t.Fatalf("n: confirm=%v requested=%v", m.restartConfirm, m.restartRequested)
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if !m.restartRequested || cmd == nil {
		t.Fatal("y did not request a restart")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("y did not quit the program")
	}
}

// TestRestartInSystemSection: "Restart ccdash" is the last setting, under
// a "system" heading that shows the running version.
func TestRestartInSystemSection(t *testing.T) {
	specs := settings.AllSpecs()
	if specs[len(specs)-1].Key != settings.KeyRestart {
		t.Fatalf("last setting is %q, want restart", specs[len(specs)-1].Key)
	}
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 100, 200
	out := ansi.Strip(m.renderSettingsBody(0))
	sys := strings.Index(out, "-- system")
	ver := strings.Index(out, "Version")
	rst := strings.Index(out, "Restart ccdash")
	if sys < 0 || !(sys < ver && ver < rst) {
		t.Fatalf("want system heading → Version → Restart, got indexes %d %d %d", sys, ver, rst)
	}
	if !strings.Contains(out[ver:rst], "dev") {
		t.Fatalf("version line missing the build version:\n%s", out[ver:rst])
	}
}

// TestUpdateFromSettings: Update ccdash checks, reports "up to date" or
// opens the notes (returning to settings), and a finished install goes
// straight to the restart confirmation.
func TestUpdateFromSettings(t *testing.T) {
	old := buildinfo.Version
	buildinfo.Version = "v0.5.0"
	t.Cleanup(func() { buildinfo.Version = old })

	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 120, 40
	m.pane = paneSettings
	for i, s := range settings.AllSpecs() {
		if s.Key == settings.KeyUpdate {
			m.settingsSel = i
		}
	}
	activate := func() tea.Cmd {
		_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		return cmd
	}

	if cmd := activate(); cmd == nil || !m.updateChecking {
		t.Fatal("Update ccdash did not start a check")
	}
	m.Update(updateCheckMsg{tag: "v0.5.0"})
	if m.updateChecking || !strings.Contains(m.flash, "up to date") || m.pane != paneSettings {
		t.Fatalf("same version: checking=%v flash=%q pane=%v", m.updateChecking, m.flash, m.pane)
	}

	activate()
	m.Update(updateCheckMsg{tag: "v0.5.1"})
	if m.pane != paneReleaseNotes || m.updateAvailable != "v0.5.1" {
		t.Fatalf("newer: pane=%v available=%q", m.pane, m.updateAvailable)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.pane != paneSettings {
		t.Fatalf("esc from notes went to pane %v, want settings", m.pane)
	}

	// Known update: the action goes straight to the notes; y installs.
	activate()
	if m.pane != paneReleaseNotes {
		t.Fatal("known update did not open the notes")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"}); cmd == nil || !m.updateRunning {
		t.Fatal("y did not start the install")
	}
	m.Update(updateDoneMsg{res: selfupdate.Result{OldVersion: "v0.5.0", NewVersion: "v0.5.1"}})
	if !m.restartConfirm || !strings.Contains(ansi.Strip(first(m.restartBox())), "Updated to v0.5.1") {
		t.Fatalf("after install: confirm=%v box=\n%s", m.restartConfirm, ansi.Strip(first(m.restartBox())))
	}

	// Dev builds don't self-update.
	buildinfo.Version = "dev"
	m.restartConfirm = false
	m.pane = paneSettings
	if cmd := activate(); cmd != nil || !strings.Contains(m.flash, "dev build") {
		t.Fatalf("dev: cmd=%v flash=%q", cmd, m.flash)
	}
}

func first(box string, _ []modalButton) string { return box }

// TestModalButtonsClickable: the buttons View records sit on the rendered
// labels, a click on one acts like its key, and a click elsewhere does
// nothing (doesn't cancel, doesn't reach the dashboard).
func TestModalButtonsClickable(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 120, 40
	m.settings.AttachEnabled = true
	m.sessions = []mdl.Session{{SessionID: "run1", Title: "busy", Status: mdl.StatusActive}}
	click := func(x, y int) tea.Cmd {
		_, cmd := m.Update(tea.MouseClickMsg(tea.Mouse{X: x, Y: y, Button: tea.MouseLeft}))
		return cmd
	}
	open := func() {
		m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m.View()
		if !m.dupConfirm || len(m.modalBtns) != 2 {
			t.Fatalf("confirm=%v buttons=%d", m.dupConfirm, len(m.modalBtns))
		}
	}

	open()
	screen := strings.Split(ansi.Strip(m.View().Content), "\n")
	for _, b := range m.modalBtns {
		label := ansi.Cut(screen[b.y], b.x0, b.x1)
		if !strings.HasPrefix(strings.Trim(label, " ><"), b.key+" ") {
			t.Fatalf("button %q covers %q", b.key, label)
		}
	}

	no, yes := m.modalBtns[1], m.modalBtns[0]
	click(0, 0) // outside the buttons: nothing happens
	if !m.dupConfirm {
		t.Fatal("a click off the buttons closed the modal")
	}
	click(no.x0, no.y)
	if m.dupConfirm {
		t.Fatal("No did not close the modal")
	}
	open()
	if cmd := click(yes.x1-1, yes.y); cmd == nil || m.dupConfirm {
		t.Fatalf("Yes: cmd=%v confirm=%v", cmd, m.dupConfirm)
	}

	// The restart confirmation's buttons work the same way.
	m.restartConfirm = true
	m.View()
	if len(m.modalBtns) != 2 {
		t.Fatalf("restart buttons=%d", len(m.modalBtns))
	}
	if cmd := click(m.modalBtns[0].x0, m.modalBtns[0].y); cmd == nil || !m.restartRequested {
		t.Fatal("Restart button did not request a restart")
	}
}

// TestModalKeyboardNav: arrows / hjkl / tab move the focus between the
// buttons, enter presses the focused one, unrelated keys are ignored.
func TestModalKeyboardNav(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 120, 40
	m.settings.AttachEnabled = true
	m.sessions = []mdl.Session{{SessionID: "run1", Title: "busy", Status: mdl.StatusActive}}
	key := func(k tea.KeyPressMsg) tea.Cmd { _, c := m.Update(k); return c }
	enter := tea.KeyPressMsg{Code: tea.KeyEnter}

	key(enter) // opens the already-running confirmation, focus on No
	if !m.dupConfirm || m.modalFocus != 1 {
		t.Fatalf("confirm=%v focus=%d, want No focused", m.dupConfirm, m.modalFocus)
	}
	db, _ := m.dupConfirmBox()
	if !strings.Contains(ansi.Strip(db), ">n  No<") {
		t.Fatalf("focused No not marked:\n%s", ansi.Strip(db))
	}
	for _, k := range []tea.KeyPressMsg{
		{Code: tea.KeyDown}, {Code: tea.KeyUp}, {Code: 'x', Text: "x"}, {Code: 'q', Text: "q"},
	} {
		key(k)
	}
	if !m.dupConfirm || m.modalFocus != 1 {
		t.Fatalf("down/up/x/q: confirm=%v focus=%d", m.dupConfirm, m.modalFocus)
	}
	key(tea.KeyPressMsg{Code: tea.KeyLeft}) // → Yes
	if m.modalFocus != 0 {
		t.Fatalf("left: focus=%d, want 0 (Yes)", m.modalFocus)
	}
	key(tea.KeyPressMsg{Code: 'l', Text: "l"}) // → No
	key(tea.KeyPressMsg{Code: 'h', Text: "h"}) // → Yes
	if cmd := key(enter); cmd == nil || m.dupConfirm {
		t.Fatalf("enter on Yes: cmd=%v confirm=%v", cmd, m.dupConfirm)
	}

	// enter on the default No cancels.
	key(enter)
	if cmd := key(enter); cmd != nil || m.dupConfirm {
		t.Fatalf("enter on No: cmd=%v confirm=%v", cmd, m.dupConfirm)
	}

	// Restart: focus starts on Restart; tab → Cancel; enter cancels.
	m.openRestartConfirm("")
	key(tea.KeyPressMsg{Code: tea.KeyTab})
	key(enter)
	if m.restartConfirm || m.restartRequested {
		t.Fatalf("tab+enter: confirm=%v requested=%v", m.restartConfirm, m.restartRequested)
	}
}
