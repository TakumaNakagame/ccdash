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
	box := ansi.Strip(m.restartBox())
	for _, want := range []string{"1 live session(s)", "STOPPED", "fix the login bug", "1 pending approval", "resume"} {
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
	if !m.restartConfirm || !strings.Contains(ansi.Strip(m.restartBox()), "Updated to v0.5.1") {
		t.Fatalf("after install: confirm=%v box=\n%s", m.restartConfirm, ansi.Strip(m.restartBox()))
	}

	// Dev builds don't self-update.
	buildinfo.Version = "dev"
	m.restartConfirm = false
	m.pane = paneSettings
	if cmd := activate(); cmd != nil || !strings.Contains(m.flash, "dev build") {
		t.Fatalf("dev: cmd=%v flash=%q", cmd, m.flash)
	}
}
