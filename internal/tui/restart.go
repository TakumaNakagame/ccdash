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

// handleKeyRestartConfirm: y / enter restarts, anything else cancels.
func (m *model) handleKeyRestartConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.restartConfirm = false
	switch msg.String() {
	case "y", "Y", "enter":
		m.restartRequested = true
		return m, tea.Quit
	}
	m.flash = "restart cancelled"
	return m, nil
}

// restartBox renders the confirmation window.
func (m *model) restartBox() string {
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
	if m.remote.Enabled {
		lines = append(lines, wrap("Relaunches this dashboard only. The remote collector keeps running — restart it on its host to pick up a new binary there.")...)
	} else {
		lines = append(lines, wrap("Stops the ccdash collector and relaunches ccdash, so a newly installed binary takes effect.")...)
		lines = append(lines, "", pendingStyle.Render("Before you restart:"))
		live := m.liveSessionTitles()
		if len(live) > 0 {
			lines = append(lines, bullet(fmt.Sprintf("%d live session(s) hosted by ccdash will be STOPPED. Any reply or tool call in progress is cut off midway:", len(live)))...)
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
		lines = append(lines, bullet("Conversations are kept: select a session and press enter to resume it.")...)
		lines = append(lines, bullet("Sessions started outside ccdash (your own terminal / tmux) are not affected.")...)
		lines = append(lines, bullet("Hook events sent during the few seconds of restart are lost.")...)
	}
	lines = append(lines, "", subtitleStyle.Render("y / enter restart · any other key cancel"))

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
	return box
}
