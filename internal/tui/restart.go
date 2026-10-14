package tui

// "Restart ccdash" (settings page action): a confirmation window drawn
// over the screen like the `n` picker, listing what a restart breaks,
// then — on y — quitting with ErrRestart so the CLI restarts the collector
// (POST /api/restart, else stops it) and re-execs the (possibly newly
// installed) binary. Where the collector can restart itself the window
// offers "When idle" first: a scheduled restart (POST /api/restart
// {"when":"idle"}) that waits until no hosted claude is busy. While one is
// pending a band under the header warns (restartBand) and the window
// offers to cancel it.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/takumanakagame/ccmanage/internal/buildinfo"
	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/selfupdate"
)

// ErrRestart is returned by Run when the operator confirmed a restart.
// The caller restarts or stops the collector (local mode) and re-execs ccdash.
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

// restartPollEvery is how often the TUI reads the scheduled restart.
const restartPollEvery = 3 * time.Second

// restartStateMsg carries GET /api/restart (or a schedule / cancel reply);
// note, when set, becomes the flash.
type restartStateMsg struct {
	st   mdl.RestartState
	err  error
	note string
}

func (m *model) fetchRestartCmd() tea.Cmd {
	return func() tea.Msg {
		st, err := m.store.RestartStatus(m.ctx)
		return restartStateMsg{st: st, err: err}
	}
}

// handleRestartState keeps the polled state; a collector without the route
// (older, or unreachable) just means no scheduled restart.
func (m *model) handleRestartState(msg restartStateMsg) {
	if msg.err != nil {
		if msg.note != "" {
			m.flash = msg.note + ": " + msg.err.Error()
		}
		m.restartState = mdl.RestartState{}
		return
	}
	m.restartState = msg.st
	if msg.note != "" {
		m.flash = msg.note
	}
}

// restartButtons are the confirmation's buttons: "When idle" first where
// the collector can schedule it, "Cancel schedule" while one is pending.
func (m *model) restartButtons() []buttonSpec {
	switch {
	case m.restartState.Pending:
		return []buttonSpec{
			{key: "x", label: "Cancel schedule", style: btnGo},
			{key: "y", label: "Restart now", style: btnDanger},
			{key: "n", label: "Close", style: btnPlain},
		}
	case m.restartState.CanRestart:
		return []buttonSpec{
			{key: "w", label: "When idle", style: btnGo},
			{key: "y", label: "Restart now", style: btnDanger},
			{key: "n", label: "Cancel", style: btnPlain},
		}
	}
	return []buttonSpec{
		{key: "y", label: "Restart", style: btnGo},
		{key: "n", label: "Cancel", style: btnPlain},
	}
}

// openRestartConfirm shows the restart confirmation, focus on its first
// button (Close while a restart is already scheduled). note is an
// optional first line ("Updated to vX…").
func (m *model) openRestartConfirm(note string) {
	m.restartNote = note
	m.restartConfirm = true
	m.modalFocus = 0
	if m.restartState.Pending {
		m.modalFocus = 2
	}
}

// handleKeyRestartConfirm: Restart quits with ErrRestart, When idle
// schedules, Cancel schedule drops it, Cancel / Close closes the window;
// arrows / hjkl move between the buttons (see resolveModalKeyOf).
func (m *model) handleKeyRestartConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	var keys []string
	for _, b := range m.restartButtons() {
		keys = append(keys, b.key)
	}
	action := m.resolveModalKeyOf(msg, keys)
	if action == "" {
		return m, nil
	}
	m.restartConfirm = false
	switch action {
	case "y":
		m.restartRequested = true
		return m, tea.Quit
	case "w":
		m.flash = "scheduling the restart…"
		return m, func() tea.Msg {
			st, err := m.store.ScheduleRestart(m.ctx, "tui", 0)
			note := "restart scheduled: ccdash restarts once no hosted session is busy (this TUI keeps its version until you relaunch it)"
			if err != nil {
				note = "schedule restart"
			}
			return restartStateMsg{st: st, err: err, note: note}
		}
	case "x":
		return m, func() tea.Msg {
			st, err := m.store.CancelRestart(m.ctx)
			note := "scheduled restart cancelled"
			if err != nil {
				note = "cancel restart"
			}
			return restartStateMsg{st: st, err: err, note: note}
		}
	}
	m.flash = "restart cancelled"
	if m.restartState.Pending {
		m.flash = ""
	}
	return m, nil
}

// restartBand is the warning line under the header while a restart is
// scheduled ("" otherwise).
func (m *model) restartBand() string {
	rs := m.restartState
	if !rs.Pending || m.width < 20 {
		return ""
	}
	var waits []string
	for _, b := range rs.Waiting {
		ref := shortID(b.SessionID)
		if b.Num > 0 {
			ref = fmt.Sprintf("#%d", b.Num)
		}
		waits = append(waits, ref+" "+b.Why)
	}
	msg := "⟳ ccdash restart scheduled"
	if len(waits) > 0 {
		msg += " — waiting for " + strings.Join(waits, " · ")
	} else {
		msg += " — nothing busy, restarting now"
	}
	msg += fmt.Sprintf(" — then %d hosted session(s) stop and resume", rs.Hosted)
	msg += "  [, → Restart ccdash: cancel]"
	return restartBandStyle.Render(runewidth.FillRight(runewidth.Truncate(msg, m.width, "…"), m.width))
}

var restartBandStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("16")).Background(lipgloss.Color("214"))

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
	if rs := m.restartState; rs.Pending {
		lines = append(lines, pendingStyle.Render(fmt.Sprintf("A restart is scheduled (since %s, gives up at %s).", rs.Since.Local().Format("15:04"), rs.Deadline.Local().Format("15:04"))))
		if len(rs.Waiting) == 0 {
			lines = append(lines, bullet("Nothing is busy — it restarts in a moment.")...)
		} else {
			lines = append(lines, bullet("Waiting for:")...)
			for _, b := range rs.Waiting {
				ref := shortID(b.SessionID)
				if b.Num > 0 {
					ref = fmt.Sprintf("#%d", b.Num)
				}
				lines = append(lines, "    - "+runewidth.Truncate(ref+" "+b.Why+"  "+b.Title, inner-6, "…"))
			}
		}
		lines = append(lines, "")
	} else if m.restartState.CanRestart {
		lines = append(lines, wrap("When idle (recommended): waits until no hosted session is working, holding an approval or a question, or waiting on something in the background — then restarts the collector and resumes them all, so nothing is cut off. ccdash, the portal and the app warn meanwhile; it gives up after 2 h.")...)
		lines = append(lines, "")
		lines = append(lines, wrap("Restart now:")...)
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
	specs := m.restartButtons()
	row, spans := buttonRow(specs, m.modalFocus)
	btns := placeButtons(spans, len(lines))
	var keys []string
	for _, b := range specs {
		keys = append(keys, b.key)
	}
	lines = append(lines, row, "", subtitleStyle.Render("←/→ or h/l select · enter choose · "+strings.Join(keys, " / ")+" · esc cancel · or click"))

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
	case !selfupdate.Newer(buildinfo.Version, msg.tag):
		m.flash = "ccdash " + buildinfo.Version + " is up to date"
		return nil
	}
	m.updateAvailable = msg.tag
	return m.openReleaseNotes()
}
