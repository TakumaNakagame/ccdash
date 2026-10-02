package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
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
