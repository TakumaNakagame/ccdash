package tui

// "Restart ccdash" (settings page action): a confirmation window drawn
// over the screen like the `n` picker, listing what a restart breaks,
// then — on y — quitting with ErrRestart so the CLI stops the collector
// and re-execs the (possibly newly installed) binary.

import (
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/takumanakagame/ccmanage/internal/buildinfo"
	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

// ErrRestart is returned by Run when the operator confirmed a restart.
// The caller stops the collector (local mode) and re-execs ccdash.
var ErrRestart = errors.New("ccdash: restart requested")

// liveSessionTitles lists the sessions whose claude runs under the ccdash
// server — the ones a collector restart stops.
func (m *model) liveSessionTitles() []string {
	var out []string
	for _, s := range m.allSessions {
		if m.ptyAlive[s.SessionID] {
			out = append(out, s.DisplayTitle())
		}
	}
	return out
}

// pendingApprovalCount is how many PermissionRequests ccdash is holding.
func (m *model) pendingApprovalCount() int {
	n := 0
	for _, a := range m.approvals {
		if a.Status == mdl.ApprovalPending {
			n++
		}
	}
	return n
}

// openRestartConfirm shows the restart confirmation, focus on Restart.
// note is an optional first line ("Updated to vX…").
func (m *model) openRestartConfirm(note string) {
	m.restartNote = note
	m.restartConfirm = true
	m.modalFocus = 0
}

// handleKeyRestartConfirm: Restart quits with ErrRestart, Cancel closes
// the window; arrows / hjkl move between the two (see resolveModalKey).
func (m *model) handleKeyRestartConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	action := m.resolveModalKey(msg)
	if action == "" {
		return m, nil
	}
	m.restartConfirm = false
	if action == "y" {
		m.restartRequested = true
		return m, tea.Quit
	}
	m.flash = "restart cancelled"
	return m, nil
}

// restartBox renders the confirmation window and its clickable buttons
// (box-relative).
func (m *model) restartBox() (string, []modalButton) {
	w := min(max(m.width-8, 40), 80)
	inner := w - 4
	wrap := func(s string) []string {
		return strings.Split(ansi.Wordwrap(s, inner-2, ""), "\n")
	}
	bullet := func(s string) []string {
		ls := wrap(s)
		for i := range ls {
			if i == 0 {
				ls[i] = "• " + ls[i]
			} else {
				ls[i] = "  " + ls[i]
			}
		}
		return ls
	}

	var lines []string
	if m.restartNote != "" {
		lines = append(lines, statusActive.Render(m.restartNote), "")
	}
	if m.remote.Enabled {
		lines = append(lines, wrap("Relaunches this dashboard only. The remote collector keeps running — restart it on its host to pick up a new binary there.")...)
	} else {
		lines = append(lines, wrap("Stops the ccdash collector and relaunches ccdash, so a newly installed binary takes effect.")...)
		lines = append(lines, "", pendingStyle.Render("Before you restart:"))
		live := m.liveSessionTitles()
		if len(live) > 0 {
			lines = append(lines, bullet(fmt.Sprintf("%d live session(s) hosted by ccdash will be stopped and resumed automatically (claude --resume). Any reply or tool call in progress is cut off midway:", len(live)))...)
			const maxShown = 5
			for i, t := range live {
				if i == maxShown {
					lines = append(lines, subtitleStyle.Render(fmt.Sprintf("    …and %d more", len(live)-maxShown)))
					break
				}
				lines = append(lines, "    - "+runewidth.Truncate(t, inner-6, "…"))
			}
		} else {
			lines = append(lines, bullet("No live sessions are hosted by ccdash right now.")...)
		}
		if n := m.pendingApprovalCount(); n > 0 {
			lines = append(lines, bullet(fmt.Sprintf("%d pending approval(s) held by ccdash will be released.", n))...)
		}
		lines = append(lines, bullet("Conversations are kept. A claude that hasn't got a session yet (no prompt sent) is not resumed.")...)
		lines = append(lines, bullet("Sessions started outside ccdash (your own terminal / tmux) are not affected.")...)
		lines = append(lines, bullet("Hook events sent during the few seconds of restart are lost.")...)
	}
	lines = append(lines, "")
	row, spans := buttonRow([]buttonSpec{
		{key: "y", label: "Restart", style: btnGo},
		{key: "n", label: "Cancel", style: btnPlain},
	}, m.modalFocus)
	btns := placeButtons(spans, len(lines))
	lines = append(lines, row, "", subtitleStyle.Render("←/→ or h/l select · enter choose · y / n · esc cancel · or click"))

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("214")).
		Padding(0, 1).
		Width(w - 2)
	const title = " restart ccdash? "
	box := style.Render(strings.Join(lines, "\n"))
	bl := strings.SplitN(box, "\n", 2)
	if len(bl) == 2 && ansi.StringWidth(bl[0]) > len(title)+4 {
		bl[0] = ansi.Cut(bl[0], 0, 2) + pendingStyle.Render(title) + ansi.TruncateLeft(bl[0], 2+len(title), "")
		box = bl[0] + "\n" + bl[1]
	}
	return box, btns
}

// openReleaseNotes shows the notes for m.updateAvailable; y there installs.
// The notes view returns to whichever pane opened it.
func (m *model) openReleaseNotes() tea.Cmd {
	m.notesReturn = m.pane
	m.pane = paneReleaseNotes
	m.updateNotes = ""
	m.updateNotesErr = nil
	m.updateNotesScroll = 0
	return m.fetchReleaseNotesCmd(m.updateAvailable)
}

// startUpdateFromSettings is the "Update ccdash" action: show the notes
// for a release already known to be newer, otherwise check now.
func (m *model) startUpdateFromSettings() tea.Cmd {
	switch {
	case buildinfo.IsDev():
		m.flash = "dev build: update with git pull + go install, then Restart ccdash"
		return nil
	case m.updateRunning:
		m.flash = "update already in progress…"
		return nil
	case m.updateChecking:
		return nil
	case m.updateAvailable != "":
		return m.openReleaseNotes()
	}
	m.updateChecking = true
	m.flash = "checking for updates…"
	return m.checkUpdateCmd()
}

// manualUpdateChecked reports a settings-triggered check and, when a newer
// release exists, goes straight to its notes.
func (m *model) manualUpdateChecked(msg updateCheckMsg) tea.Cmd {
	m.updateChecking = false
	switch {
	case msg.err != nil:
		m.flash = "update check failed: " + msg.err.Error()
		return nil
	case msg.tag == "" || msg.tag == buildinfo.Version:
		m.flash = "ccdash " + buildinfo.Version + " is up to date"
		return nil
	}
	m.updateAvailable = msg.tag
	return m.openReleaseNotes()
}
