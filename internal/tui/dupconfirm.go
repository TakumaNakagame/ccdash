package tui

// "Already running" confirmation: opening (enter / double-click) a session
// whose claude is alive in another terminal would resume it here as a
// second claude on the same conversation. Ask first, in a centered window
// like the `n` picker; only an explicit y goes ahead.

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

// runningElsewhere reports whether s has a live claude that ccdash doesn't
// host (and that isn't in a tmux pane enter could switch to).
func (m *model) runningElsewhere(s mdl.Session) bool {
	if s.Pane != "" || m.ptyAlive[s.SessionID] {
		return false
	}
	return s.Status == mdl.StatusActive || s.Status == mdl.StatusIdle
}

// openDupConfirm asks before opening s, which runs in another terminal.
// The focus starts on No, so a reflexive enter doesn't open a second one.
func (m *model) openDupConfirm(s mdl.Session) {
	m.dupSessionID = s.SessionID
	m.dupConfirm = true
	m.modalFocus = 1
}

// handleKeyDupConfirm: Yes opens the session anyway, No closes the window;
// arrows / hjkl move between the two (see resolveModalKey).
func (m *model) handleKeyDupConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	action := m.resolveModalKey(msg)
	if action == "" {
		return m, nil
	}
	sid := m.dupSessionID
	m.dupConfirm = false
	m.dupSessionID = ""
	if action != "y" {
		m.flash = "not opened"
		return m, nil
	}
	for i, s := range m.sessions {
		if s.SessionID == sid {
			m.selSess = i
			return m, m.attachSession(true)
		}
	}
	m.flash = "session is no longer listed"
	return m, nil
}

// dupConfirmBox renders the confirmation window and its clickable
// buttons (box-relative).
func (m *model) dupConfirmBox() (string, []modalButton) {
	w := min(max(m.width-8, 40), 80)
	inner := w - 4
	wrap := func(s string) []string {
		return strings.Split(ansi.Wordwrap(s, inner, ""), "\n")
	}
	var s mdl.Session
	for _, c := range m.sessions {
		if c.SessionID == m.dupSessionID {
			s = c
		}
	}
	where := "another terminal"
	if s.ProcPID != 0 {
		where += fmt.Sprintf(" (pid %d)", s.ProcPID)
	}

	lines := []string{
		pendingStyle.Render(runewidth.Truncate(s.DisplayTitle(), inner, "…")),
		subtitleStyle.Render(runewidth.Truncate(tildePath(s.Cwd)+"  "+shortID(s.SessionID), inner, "…")),
		"",
	}
	lines = append(lines, wrap("This session is already open in "+where+".")...)
	lines = append(lines, "")
	lines = append(lines, wrap("Opening it here starts a second claude on the same conversation. Both write to the same transcript, so their replies can interleave and the history gets confusing.")...)
	lines = append(lines, "", "Open it here anyway?", "")
	row, spans := buttonRow([]buttonSpec{
		{key: "y", label: "Yes, open a second one", style: btnDanger},
		{key: "n", label: "No", style: btnPlain},
	}, m.modalFocus)
	btns := placeButtons(spans, len(lines))
	lines = append(lines, row, "", subtitleStyle.Render("←/→ or h/l select · enter choose · y / n · esc cancel · or click"))

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("196")).
		Padding(0, 1).
		Width(w - 2)
	const title = " already running "
	box := style.Render(strings.Join(lines, "\n"))
	bl := strings.SplitN(box, "\n", 2)
	if len(bl) == 2 && ansi.StringWidth(bl[0]) > len(title)+4 {
		bl[0] = ansi.Cut(bl[0], 0, 2) + pendingStyle.Render(title) + ansi.TruncateLeft(bl[0], 2+len(title), "")
		box = bl[0] + "\n" + bl[1]
	}
	return box, btns
}
