package tui

// Mouse selection in the session list: a click selects the session (the
// right pane previews it), a double click on the same row opens it the way
// enter would — but only where that can't surprise anyone: a stopped
// session is resumed, one ccdash already hosts gets the keyboard, and one
// running in another terminal / tmux is left alone (a click must not spawn
// a second claude on a live session or yank the tmux client away).

import (
	"time"

	tea "charm.land/bubbletea/v2"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

// doubleClickWindow is the max gap between the two clicks of a double click.
const doubleClickWindow = 400 * time.Millisecond

// modalOpen reports whether a prompt, picker, or confirmation owns input,
// in which case clicks on the dashboard behind it are ignored.
func (m *model) modalOpen() bool {
	return m.pane != paneSessions ||
		m.editingTitle || m.editingGroup || m.editingSearch ||
		m.editingNewSession || m.editingSkill || m.restartConfirm ||
		m.awaitGroupArchiveConfirm || m.awaitSummaryConfirm || m.awaitMkdirConfirm
}

// listSessionAt maps a terminal position to the session row under it, or
// -1 when it's not on a session (a date header, the footer line, the right
// pane, or outside the body).
func (m *model) listSessionAt(mm tea.Mouse) int {
	if m.modalOpen() || m.mouseInRightPane(mm) {
		return -1
	}
	bodyTop, _ := m.bodyLayout()
	if !m.useVerticalLayout() && mm.X >= m.leftPaneWidth() {
		return -1 // the separator columns
	}
	line := mm.Y - bodyTop
	if line < 0 || line >= len(m.listLineSess) {
		return -1
	}
	idx := m.listLineSess[line]
	if idx >= len(m.sessions) {
		return -1
	}
	return idx
}

// clickSessionList handles a left click that didn't land in the live
// screen.
func (m *model) clickSessionList(mm tea.Mouse) tea.Cmd {
	idx := m.listSessionAt(mm)
	if idx < 0 {
		return nil
	}
	now := time.Now()
	double := idx == m.lastClickIdx && now.Sub(m.lastClickAt) <= doubleClickWindow
	if double {
		// A third click starts a new pair rather than re-firing.
		m.lastClickAt = time.Time{}
	} else {
		m.lastClickIdx, m.lastClickAt = idx, now
	}

	if idx != m.selSess {
		return m.jumpTo(idx)
	}
	if !double {
		return nil
	}
	return m.openSessionByClick()
}

// openSessionByClick is the double-click action on the selected session.
func (m *model) openSessionByClick() tea.Cmd {
	s := m.sessions[m.selSess]
	if live := m.liveForCurrent(); live != nil && !live.exited {
		m.setLiveFocus(true)
		return nil
	}
	if m.ptyAlive[s.SessionID] {
		return nil // hosted here; the live screen is still connecting
	}
	if s.Pane != "" || s.Status == mdl.StatusActive || s.Status == mdl.StatusIdle {
		m.flash = "running in another terminal — press enter to switch / attach"
		return nil
	}
	if !m.settings.AttachEnabled {
		m.flash = "attach is OFF (settings ',')"
		return nil
	}
	return m.attachCurrent()
}
