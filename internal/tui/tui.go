package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/takumanakagame/ccmanage/internal/attach"
	"github.com/takumanakagame/ccmanage/internal/auth"
	"github.com/takumanakagame/ccmanage/internal/buildinfo"
	"github.com/takumanakagame/ccmanage/internal/db"
	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/paths"
	"github.com/takumanakagame/ccmanage/internal/settings"
	"github.com/takumanakagame/ccmanage/internal/summarize"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

func Run(ctx context.Context, d *db.DB, lockGroup string) error {
	m := newModel(ctx, d)
	if s, err := settings.Load(ctx, d); err == nil {
		m.settings = s
	}
	if lockGroup != "" {
		m.groupFilter = lockGroup
		m.groupLocked = true
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

type pane int

const (
	paneSessions pane = iota
	paneTranscript
	paneSettings
)

type model struct {
	ctx        context.Context
	db         *db.DB
	width      int
	height     int
	allSessions []mdl.Session // unfiltered, latest from DB
	sessions    []mdl.Session // post-project-filter — what the list actually renders
	events     []mdl.Event
	approvals  []mdl.Approval
	selSess    int
	selAppr    int
	sessScroll int
	pane       pane
	err        error
	flash      string
	lastTick   time.Time

	// pending-approval alert state
	lastPendingTotal int
	bellPrimed       bool // skip bell on the very first refresh
	pendingBell      bool // set on transition; flushed once via View()

	// inline transcript tail for the right pane
	tailMessages []transcript.Message
	tailPath     string
	tailMtime    time.Time
	// tailScroll offsets the right-pane view from the latest line. 0 means
	// "auto-scroll to newest"; positive means "show this many lines older".
	tailScroll int

	// list mode + inline editor state
	showArchived  bool
	editingTitle  bool
	editingGroup    bool
	editingSearch bool
	titleBuffer   string
	groupCandIdx    int    // index into filteredGroupCandidates(); -1 == "no pick yet"
	groupFilter string // "" = All; otherwise repo / cwd basename
	searchQuery   string // "" = no filter; case-insensitive substring search

	settings settings.Settings

	// awaitGroupArchiveConfirm is true after the operator pressed ctrl+x
	// to bulk-archive the current tab; the next keystroke is treated as
	// the y/n confirmation rather than a normal shortcut.
	awaitGroupArchiveConfirm bool
	// awaitSummaryConfirm is the same gate for the 's' summarize
	// shortcut; spawning claude -p costs an API round trip so we don't
	// want it to fire on a typo.
	awaitSummaryConfirm bool

	// awaitMkdirConfirm guards the mkdir step when the operator hits
	// Enter on a path that doesn't exist. y → create + start; anything
	// else cancels. pendingMkdirPath holds the absolute path we'd
	// create if the operator confirms.
	awaitMkdirConfirm bool
	pendingMkdirPath  string

	// groupLocked is set when the operator launched ccdash with --tab
	// <name>. The tab strip is hidden, h/l/Tab/Shift+Tab become no-ops,
	// and the groupFilter never changes.
	groupLocked bool

	// settings page state
	settingsSel    int
	settingsEdit   bool   // editing an int value inline
	settingsBuffer string // numeric buffer while editing

	// transcript view state
	transcriptMessages []transcript.Message
	transcriptScroll   int
	transcriptTitle    string
	transcriptPath     string

	// attached holds long-lived attach.Session instances keyed by session
	// id. Detaching with Ctrl+D leaves the entry in place so the next
	// Enter resumes the same child instead of spawning a new one. Sessions
	// whose child has died are pruned at attach time.
	attached map[string]*attach.Session

	// attachedByPID is the staging area for sessions we spawn ourselves
	// (via 'n' new-session). The session id only becomes known after
	// claude writes its first JSONL line and discovery picks it up — until
	// then we key by PID. The promotion to `attached` happens in
	// promoteAttached, called from refresh.
	attachedByPID map[int]*attach.Session

	// animTick advances on every animTickMsg (~150 ms). Drives the spinner
	// frame for active rows so the operator can tell at a glance which
	// sessions are doing work right now. Wraps naturally — we mod into the
	// frame slice on read.
	animTick int

	// editingNewSession is true while the operator is typing a directory
	// path in the new-session footer prompt (entered via 'n').
	editingNewSession bool
	newSessionBuffer  string
	// newSessionCompletions caches the directory expansion for the current
	// buffer prefix; cycled by repeated Tab presses.
	newSessionCompletions []string
	newSessionCompIdx     int
}

func newModel(ctx context.Context, d *db.DB) *model {
	return &model{
		ctx:           ctx,
		db:            d,
		settings:      settings.Defaults(),
		attached:      map[string]*attach.Session{},
		attachedByPID: map[int]*attach.Session{},
	}
}

// quit tears down any backgrounded attach.Sessions before returning
// tea.Quit, so we don't leave orphan claude processes attached to a PTY
// whose only owner just exited. Each Close sends SIGTERM and waits — fast
// in practice (claude flushes its JSONL and exits quickly) but bounded by
// the kernel's process-exit semantics either way.
func (m *model) quit() tea.Cmd {
	for sid, s := range m.attached {
		_ = s.Close()
		delete(m.attached, sid)
	}
	for pid, s := range m.attachedByPID {
		_ = s.Close()
		delete(m.attachedByPID, pid)
	}
	return tea.Quit
}

// promoteAttached walks freshly-loaded session rows and links any pending
// PID-keyed attach.Session into the SessionID-keyed map. Called from the
// sessionsMsg handler so that the moment discovery picks up a new session
// the row's Enter key wires through to the same child the operator just
// detached from.
func (m *model) promoteAttached() {
	if len(m.attachedByPID) == 0 {
		return
	}
	for _, s := range m.allSessions {
		if s.ProcPID == 0 || s.SessionID == "" {
			continue
		}
		sess, ok := m.attachedByPID[s.ProcPID]
		if !ok {
			continue
		}
		if existing, already := m.attached[s.SessionID]; !already || existing != sess {
			// Existing entries by id win — there's no safe automatic
			// merge of two different live PTYs onto the same id.
			if !already {
				m.attached[s.SessionID] = sess
			}
		}
		delete(m.attachedByPID, s.ProcPID)
	}
}

// startNewSession is the front door for the `n` flow's final Enter.
// It expands the path, checks whether the directory exists, and either:
//   - immediately spawns claude (if the dir is fine), or
//   - parks the path on awaitMkdirConfirm so the next keystroke can
//     decide whether to create it.
// Real mkdir + spawn live in spawnNewSession, which is also reused
// directly when the confirmation gate fires.
func (m *model) startNewSession(dir string) tea.Cmd {
	if dir == "" {
		return func() tea.Msg { return attachDoneMsg{err: fmt.Errorf("path required")} }
	}
	expanded, err := expandPath(dir)
	if err != nil {
		return func() tea.Msg { return attachDoneMsg{err: err} }
	}
	fi, err := os.Stat(expanded)
	switch {
	case err == nil && !fi.IsDir():
		return func() tea.Msg { return attachDoneMsg{err: fmt.Errorf("not a directory: %s", expanded)} }
	case err == nil:
		return m.spawnNewSession(expanded, false)
	case os.IsNotExist(err):
		// Don't mkdir silently — a typo on the last segment would create
		// a junk dir without the operator noticing. Park the path and
		// surface a confirmation banner; the next keystroke handler
		// decides whether to commit.
		m.awaitMkdirConfirm = true
		m.pendingMkdirPath = expanded
		m.flash = fmt.Sprintf("'%s' does not exist — press 'y' to create + start, any other key to cancel", expanded)
		return nil
	default:
		return func() tea.Msg { return attachDoneMsg{err: fmt.Errorf("stat %s: %w", expanded, err)} }
	}
}

// spawnNewSession does the actual mkdir-if-needed-and-claude-launch dance.
// `created` controls the post-detach flash so the operator knows whether
// ccdash made a new directory on their behalf.
func (m *model) spawnNewSession(expanded string, created bool) tea.Cmd {
	if created {
		if mkErr := os.MkdirAll(expanded, 0o755); mkErr != nil {
			return func() tea.Msg { return attachDoneMsg{err: fmt.Errorf("mkdir %s: %w", expanded, mkErr)} }
		}
	}
	c := exec.Command("claude")
	c.Dir = expanded
	sess := attach.New(c)
	if err := sess.Start(); err != nil {
		return func() tea.Msg { return attachDoneMsg{err: err} }
	}
	m.attachedByPID[sess.PID()] = sess
	ac := &attach.AttachCmd{Session: sess}
	cwd := expanded
	createdNote := ""
	if created {
		createdNote = " (created)"
	}
	return tea.Exec(ac, func(err error) tea.Msg {
		if err != nil {
			return attachDoneMsg{err: err}
		}
		switch {
		case ac.Result.Detached:
			return attachDoneMsg{msg: "detached — new claude session running in " + cwd + createdNote + " (Enter on the row to reattach)"}
		case ac.Result.ExitErr != nil:
			return attachDoneMsg{err: ac.Result.ExitErr, msg: "claude session ended"}
		default:
			return attachDoneMsg{msg: "claude session ended"}
		}
	})
}

// expandPath replaces a leading ~ with the operator's home dir and
// resolves to an absolute path. Empty input is rejected upstream.
func expandPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			p = home
		} else if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// completeDirPrefix lists subdirectories whose name starts with the
// final segment of `buf`. Returns the candidates (full-path form, with
// a trailing slash) so the caller can substitute them into the input.
// Quiet on errors — completion is best-effort.
func completeDirPrefix(buf string) []string {
	expanded, err := expandPath(buf)
	if err != nil {
		return nil
	}
	dir := expanded
	prefix := ""
	if !strings.HasSuffix(buf, "/") && !strings.HasSuffix(buf, string(filepath.Separator)) {
		dir = filepath.Dir(expanded)
		prefix = filepath.Base(expanded)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			// Skip dotdirs unless the operator is explicitly asking for one.
			continue
		}
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		out = append(out, filepath.Join(dir, name)+string(filepath.Separator))
	}
	return out
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tickCmd(m.tickInterval()), animTickCmd())
}

type tickMsg time.Time
type animTickMsg time.Time

type sessionsMsg []mdl.Session
type eventsMsg []mdl.Event
type approvalsMsg []mdl.Approval
type errMsg struct{ err error }

func tickCmd(every time.Duration) tea.Cmd {
	if every <= 0 {
		every = time.Second
	}
	return tea.Tick(every, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// animTickCmd schedules the next spinner frame. 150 ms feels lively without
// drowning the terminal in repaints — Bubble Tea coalesces renders, but
// every tick also rebuilds the view tree, so we don't want to push it
// faster than the eye actually resolves.
func animTickCmd() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg { return animTickMsg(t) })
}

func (m *model) tickInterval() time.Duration {
	if ms := m.settings.RefreshIntervalMs; ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return time.Second
}

func (m *model) refresh() tea.Cmd {
	archived := m.showArchived
	cmds := []tea.Cmd{
		func() tea.Msg {
			ss, err := m.db.ListSessions(m.ctx, archived)
			if err != nil {
				return errMsg{err}
			}
			return sessionsMsg(ss)
		},
		func() tea.Msg {
			as, err := m.db.ListPendingApprovals(m.ctx)
			if err != nil {
				return errMsg{err}
			}
			return approvalsMsg(as)
		},
	}
	if cmd := m.loadTailCmd(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// loadTailCmd returns a Cmd that (re)loads the inline transcript for the
// currently selected session. It avoids reparsing if the file's mtime hasn't
// advanced since the last load — most ticks land on an unchanged file.
func (m *model) loadTailCmd() tea.Cmd {
	if len(m.sessions) == 0 || m.selSess < 0 || m.selSess >= len(m.sessions) {
		return nil
	}
	s := m.sessions[m.selSess]
	path := s.TranscriptPath
	if path == "" {
		return nil
	}
	prevPath := m.tailPath
	prevMtime := m.tailMtime
	return func() tea.Msg {
		fi, err := os.Stat(path)
		if err != nil {
			return tailMsg{path: path, err: err}
		}
		if path == prevPath && !fi.ModTime().After(prevMtime) {
			return tailMsg{path: path, mtime: fi.ModTime(), unchanged: true}
		}
		// Tail-read keeps session-switching fast even for transcripts in
		// the tens of megabytes; we only need the recent end for the
		// inline pane (the modal viewer pulls the full file).
		budgetKB := m.settings.TailBudgetKB
		if budgetKB <= 0 {
			budgetKB = 256
		}
		msgs, err := transcript.LoadTail(path, int64(budgetKB)*1024)
		if err != nil {
			return tailMsg{path: path, mtime: fi.ModTime(), err: err}
		}
		return tailMsg{path: path, mtime: fi.ModTime(), messages: msgs}
	}
}

type tailMsg struct {
	path      string
	mtime     time.Time
	messages  []transcript.Message
	unchanged bool
	err       error
}

func (m *model) currentSessionID() string {
	if len(m.sessions) == 0 || m.selSess < 0 || m.selSess >= len(m.sessions) {
		return ""
	}
	return m.sessions[m.selSess].SessionID
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		m.lastTick = time.Time(msg)
		return m, tea.Batch(m.refresh(), tickCmd(m.tickInterval()))
	case animTickMsg:
		m.animTick++
		return m, animTickCmd()
	case sessionsMsg:
		prev := m.currentSessionID()
		m.allSessions = []mdl.Session(msg)
		// Promote any newly-discovered sessions whose PID matches a
		// freshly-spawned attach.Session into the by-id map so that
		// pressing Enter on the row reattaches the same child instead of
		// starting a competing `claude --resume`.
		m.promoteAttached()
		// If the tab the operator was looking at vanished (its last
		// session got archived, removed, or moved to a different tab),
		// step onto the next available tab so the body isn't blank.
		// --tab pinned operators stay put; that's their explicit ask.
		if !m.groupLocked && m.groupFilter != "" {
			projects := m.uniqueGroups()
			present := false
			for _, p := range projects {
				if p == m.groupFilter {
					present = true
					break
				}
			}
			if !present {
				from := m.groupFilter
				next := ""
				if len(projects) > 1 {
					next = projects[1]
				}
				m.groupFilter = next
				if next == "" {
					m.flash = fmt.Sprintf("'%s' tab emptied → All", from)
				} else {
					m.flash = fmt.Sprintf("'%s' tab emptied → '%s'", from, next)
				}
				m.tailPath = ""
				m.tailMtime = time.Time{}
			}
		}
		m.applyGroupFilter()
		// Try to keep the cursor on the same session as before; only
		// fall back to defaultSelectionIdx (newest) when the previous
		// session is no longer present (filter change, archive toggle,
		// session vanished, etc.).
		found := false
		if prev != "" {
			for i, s := range m.sessions {
				if s.SessionID == prev {
					m.selSess = i
					found = true
					break
				}
			}
		}
		if !found {
			m.selSess = m.defaultSelectionIdx()
		}
		if m.selSess >= len(m.sessions) {
			m.selSess = len(m.sessions) - 1
		}
		if m.selSess < 0 {
			m.selSess = 0
		}
		if cur := m.currentSessionID(); cur != "" && cur != prev {
			return m, m.loadTailCmd()
		}
	case eventsMsg:
		m.events = []mdl.Event(msg)
	case tailMsg:
		if msg.err != nil {
			m.err = msg.err
			break
		}
		m.tailPath = msg.path
		m.tailMtime = msg.mtime
		if !msg.unchanged {
			m.tailMessages = msg.messages
		}
	case approvalsMsg:
		m.approvals = []mdl.Approval(msg)
		if m.selAppr >= len(m.approvals) {
			m.selAppr = len(m.approvals) - 1
		}
		if m.selAppr < 0 {
			m.selAppr = 0
		}
		// Ring the terminal bell once when pending count goes from zero to
		// non-zero. We emit \a as part of the rendered View so it goes
		// through Bubble Tea's writer rather than competing with the alt
		// screen via stderr.
		total := len(m.approvals)
		if m.settings.BellOnPending && m.bellPrimed && m.lastPendingTotal == 0 && total > 0 {
			m.pendingBell = true
		}
		m.lastPendingTotal = total
		m.bellPrimed = true
	case errMsg:
		m.err = msg.err
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case attachDoneMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.flash = msg.msg
		}
	case summaryDoneMsg:
		if msg.err != nil {
			_ = m.db.SetSummary(m.ctx, msg.sessionID, msg.err.Error(), "error")
			m.flash = "summary failed: " + msg.err.Error()
		} else {
			_ = m.db.SetSummary(m.ctx, msg.sessionID, msg.summary, "done")
			m.flash = "summary updated"
		}
		return m, m.refresh()
	case transcriptLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.transcriptMessages = msg.messages
		m.transcriptPath = msg.path
		m.transcriptTitle = msg.title
		m.transcriptScroll = m.maxTranscriptScroll(m.transcriptVisibleHeight())
		m.pane = paneTranscript
		m.err = nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleMouse routes wheel events. In the modal transcript view, scrolling
// always moves through that buffer. In the sessions view, the wheel zone
// is decided by Y in vertical layout (top = sessions, bottom = transcript)
// or by X in horizontal layout (left = sessions, right = transcript).
func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	const wheelStep = 3
	if m.pane == paneTranscript {
		switch msg.Type {
		case tea.MouseWheelUp:
			bodyHeight := m.transcriptVisibleHeight()
			m.transcriptScroll = clamp(m.transcriptScroll-wheelStep, 0, m.maxTranscriptScroll(bodyHeight))
		case tea.MouseWheelDown:
			bodyHeight := m.transcriptVisibleHeight()
			m.transcriptScroll = clamp(m.transcriptScroll+wheelStep, 0, m.maxTranscriptScroll(bodyHeight))
		}
		return m, nil
	}
	inRight := m.mouseInRightPane(msg)
	switch msg.Type {
	case tea.MouseWheelUp:
		if inRight {
			m.tailScroll += wheelStep
			return m, nil
		}
		return m, m.move(-1)
	case tea.MouseWheelDown:
		if inRight {
			m.tailScroll -= wheelStep
			if m.tailScroll < 0 {
				m.tailScroll = 0
			}
			return m, nil
		}
		return m, m.move(1)
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.pane == paneTranscript {
		return m.handleKeyTranscript(msg)
	}
	if m.pane == paneSettings {
		return m.handleKeySettings(msg)
	}
	if m.editingSearch {
		return m.handleKeySearchEdit(msg)
	}
	if m.editingNewSession {
		return m.handleKeyNewSessionEdit(msg)
	}
	if m.editingTitle || m.editingGroup {
		return m.handleKeyTitleEdit(msg)
	}
	if m.awaitGroupArchiveConfirm {
		m.awaitGroupArchiveConfirm = false
		switch msg.String() {
		case "y", "Y":
			return m, m.archiveCurrentGroup()
		default:
			m.flash = "group archive cancelled"
			return m, nil
		}
	}
	if m.awaitSummaryConfirm {
		m.awaitSummaryConfirm = false
		switch msg.String() {
		case "y", "Y":
			return m, m.summarizeCurrent()
		default:
			m.flash = "summary cancelled"
			return m, nil
		}
	}
	if m.awaitMkdirConfirm {
		m.awaitMkdirConfirm = false
		path := m.pendingMkdirPath
		m.pendingMkdirPath = ""
		switch msg.String() {
		case "y", "Y":
			m.flash = ""
			return m, m.spawnNewSession(path, true)
		default:
			m.flash = "new session cancelled"
			return m, nil
		}
	}
	switch msg.String() {
	case "ctrl+c", "q":
		return m, m.quit()
	case "j", "down":
		return m, m.move(1)
	case "k", "up":
		return m, m.move(-1)
	case "g", "home":
		return m, m.jumpTo(0)
	case "G", "end":
		return m, m.jumpTo(-1)
	case "J":
		// 1-line scroll of the right-pane transcript toward the newest.
		// tailScroll==0 means "auto-tail at the bottom"; we never go
		// negative.
		if m.tailScroll > 0 {
			m.tailScroll--
		}
		return m, nil
	case "K":
		// 1-line scroll toward older content.
		m.tailScroll++
		return m, nil
	case "pgdown":
		step := m.tailHalfPage()
		m.tailScroll -= step
		if m.tailScroll < 0 {
			m.tailScroll = 0
		}
		return m, nil
	case "pgup":
		m.tailScroll += m.tailHalfPage()
		return m, nil
	case "r":
		return m, m.refresh()
	case "enter":
		if !m.settings.AttachEnabled {
			m.flash = "attach is OFF (settings ',')"
			return m, nil
		}
		return m, m.attachCurrent()
	case "o":
		return m, m.openTranscript()
	case "a":
		if !m.settings.ApproveEnabled {
			m.flash = "approval blocking is OFF (settings ',')"
			return m, nil
		}
		return m, m.decideApproval("allow", false)
	case "A":
		if !m.settings.ApproveEnabled {
			m.flash = "approval blocking is OFF (settings ',')"
			return m, nil
		}
		return m, m.decideApproval("allow", true)
	case "d":
		if !m.settings.ApproveEnabled {
			m.flash = "approval blocking is OFF (settings ',')"
			return m, nil
		}
		return m, m.decideApproval("deny", false)
	case "x":
		return m, m.toggleArchiveCurrent()
	case "ctrl+x":
		return m, m.promptArchiveCurrentGroup()
	case "X":
		m.showArchived = !m.showArchived
		m.selSess = 0
		m.tailScroll = 0
		// The fresh sessionsMsg from refresh() will reset selSess to
		// defaultSelectionIdx() when the previous selection is gone.
		return m, m.refresh()
	case "tab", "l":
		if m.groupLocked {
			m.flash = "group is locked via --group flag"
			return m, nil
		}
		return m, m.cycleGroup(1)
	case "shift+tab", "h":
		if m.groupLocked {
			m.flash = "group is locked via --group flag"
			return m, nil
		}
		return m, m.cycleGroup(-1)
	case "R":
		next, err := settings.Set(m.ctx, m.db, m.settings, "auto_repo_tabs", !m.settings.AutoRepoTabs)
		if err != nil {
			m.err = err
			return m, nil
		}
		m.settings = next
		// If we just turned auto repos off and the current filter is one
		// of them, drop it back to All so the operator isn't stuck with
		// a filter that isn't reachable from the cycle anymore.
		if !m.settings.AutoRepoTabs && m.groupFilter != "" {
			projs := m.uniqueGroups()
			present := false
			for _, p := range projs {
				if p == m.groupFilter {
					present = true
					break
				}
			}
			if !present {
				m.groupFilter = ""
				m.applyGroupFilter()
			}
		}
		if m.settings.AutoRepoTabs {
			m.flash = "auto repo tabs ON"
		} else {
			m.flash = "auto repo tabs OFF (user-named only)"
		}
		return m, nil
	case "f":
		return m, m.toggleFavoriteCurrent()
	case "t":
		return m, m.startTitleEdit()
	case "T":
		return m, m.startGroupEdit()
	case "s":
		if !m.settings.SummaryEnabled {
			m.flash = "summarize is OFF (settings ',')"
			return m, nil
		}
		if len(m.sessions) == 0 {
			return m, nil
		}
		s := m.sessions[m.selSess]
		title := s.DisplayTitle()
		if title == "" {
			title = shortID(s.SessionID)
		}
		m.awaitSummaryConfirm = true
		m.flash = fmt.Sprintf("run claude -p summary on '%s'? press 'y' to confirm", shorten(title, 60))
		return m, nil
	case ",":
		m.pane = paneSettings
		m.settingsSel = 0
		m.settingsEdit = false
		return m, nil
	case "/":
		m.editingSearch = true
		m.titleBuffer = m.searchQuery
		return m, nil
	case "n":
		// Start a new claude session by typing a directory path. Default
		// to ~/ so the operator only has to extend, not start over.
		if !m.settings.AttachEnabled {
			m.flash = "attach is OFF (settings ',')"
			return m, nil
		}
		if home, err := os.UserHomeDir(); err == nil {
			m.newSessionBuffer = home + string(filepath.Separator)
		} else {
			m.newSessionBuffer = ""
		}
		m.newSessionCompletions = nil
		m.newSessionCompIdx = -1
		m.editingNewSession = true
		return m, nil
	case "esc":
		if m.searchQuery != "" {
			m.searchQuery = ""
			m.applyGroupFilter()
			m.selSess = 0
			m.flash = "search cleared"
		}
		return m, nil
	}
	return m, nil
}

// applyGroupFilter recomputes m.sessions from m.allSessions using the
// current groupFilter and searchQuery. The two filters compose by
// intersection.
func (m *model) applyGroupFilter() {
	src := m.allSessions
	if m.groupFilter != "" {
		out := make([]mdl.Session, 0, len(src))
		for _, s := range src {
			if groupOf(s) == m.groupFilter {
				out = append(out, s)
			}
		}
		src = out
	}
	if m.searchQuery != "" {
		q := strings.ToLower(m.searchQuery)
		out := make([]mdl.Session, 0, len(src))
		for _, s := range src {
			if sessionMatchesQuery(s, q) {
				out = append(out, s)
			}
		}
		src = out
	}
	if m.settings.NewestAtBottom {
		// Reverse in place — caller no longer relies on the original
		// ordering of m.sessions, just on the displayed indices.
		for i, j := 0, len(src)-1; i < j; i, j = i+1, j-1 {
			src[i], src[j] = src[j], src[i]
		}
	}
	m.sessions = src
}

// sessionMatchesQuery returns true when q (lower-cased) appears in any of
// the human-readable fields of s. We don't search payloads or transcripts
// to keep the per-keystroke cost predictable.
func sessionMatchesQuery(s mdl.Session, q string) bool {
	for _, f := range []string{
		s.DisplayTitle(), s.UserGroup, s.Repo, s.Cwd, s.Branch,
		s.Summary, s.SessionID,
	} {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// handleKeySearchEdit processes keys while the / search input is active.
func (m *model) handleKeySearchEdit(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		m.searchQuery = strings.TrimSpace(m.titleBuffer)
		m.editingSearch = false
		m.titleBuffer = ""
		m.applyGroupFilter()
		m.selSess = 0
		m.sessScroll = 0
		return m, m.loadTailCmd()
	case tea.KeyEsc, tea.KeyCtrlC:
		m.editingSearch = false
		m.titleBuffer = ""
		return m, nil
	case tea.KeyBackspace:
		if r := []rune(m.titleBuffer); len(r) > 0 {
			m.titleBuffer = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		m.titleBuffer += " "
	case tea.KeyRunes:
		m.titleBuffer += string(msg.Runes)
	}
	return m, nil
}

// handleKeyNewSessionEdit drives the footer prompt for `n`. The buffer is
// a directory path. UX:
//   - Tab          : cycle highlight through live candidates (preview only,
//                    buffer stays put so the operator can keep typing).
//   - / or Enter   : when a candidate is highlighted, commit it into the
//                    buffer and reset the highlight — Enter then "descends"
//                    one more level on the next press.
//   - Enter        : with no highlight, starts a `claude` session in the
//                    buffer path (creates the dir if missing).
//   - Esc          : cancel.
func (m *model) handleKeyNewSessionEdit(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	hasHighlight := m.newSessionCompIdx >= 0 && m.newSessionCompIdx < len(m.newSessionCompletions)

	commitHighlight := func() {
		m.newSessionBuffer = m.newSessionCompletions[m.newSessionCompIdx]
		m.newSessionCompletions = nil
		m.newSessionCompIdx = -1
	}

	switch msg.Type {
	case tea.KeyEnter:
		if hasHighlight {
			commitHighlight()
			return m, nil
		}
		dir := m.newSessionBuffer
		m.editingNewSession = false
		m.newSessionBuffer = ""
		m.newSessionCompletions = nil
		m.newSessionCompIdx = -1
		return m, m.startNewSession(dir)
	case tea.KeyEsc, tea.KeyCtrlC:
		m.editingNewSession = false
		m.newSessionBuffer = ""
		m.newSessionCompletions = nil
		m.newSessionCompIdx = -1
		return m, nil
	case tea.KeyTab:
		// Tab is preview only — recompute against the current buffer and
		// advance the highlight index. The buffer is untouched until the
		// operator commits with `/` or Enter.
		m.newSessionCompletions = completeDirPrefix(m.newSessionBuffer)
		if len(m.newSessionCompletions) == 0 {
			m.flash = "no directory matches"
			m.newSessionCompIdx = -1
			return m, nil
		}
		m.newSessionCompIdx = (m.newSessionCompIdx + 1) % len(m.newSessionCompletions)
		return m, nil
	case tea.KeyShiftTab:
		m.newSessionCompletions = completeDirPrefix(m.newSessionBuffer)
		if len(m.newSessionCompletions) == 0 {
			m.flash = "no directory matches"
			m.newSessionCompIdx = -1
			return m, nil
		}
		if m.newSessionCompIdx <= 0 {
			m.newSessionCompIdx = len(m.newSessionCompletions) - 1
		} else {
			m.newSessionCompIdx--
		}
		return m, nil
	case tea.KeyBackspace:
		if r := []rune(m.newSessionBuffer); len(r) > 0 {
			m.newSessionBuffer = string(r[:len(r)-1])
		}
		m.newSessionCompletions = nil
		m.newSessionCompIdx = -1
	case tea.KeySpace:
		m.newSessionBuffer += " "
		m.newSessionCompletions = nil
		m.newSessionCompIdx = -1
	case tea.KeyRunes:
		// "/" is overloaded: when a candidate is highlighted it commits
		// (just like Enter on a highlight). Otherwise it's just another
		// path-separator character.
		if string(msg.Runes) == "/" && hasHighlight {
			commitHighlight()
			return m, nil
		}
		m.newSessionBuffer += string(msg.Runes)
		m.newSessionCompletions = nil
		m.newSessionCompIdx = -1
	}
	return m, nil
}

// groupOf names the group a session belongs to. Operator-set user_group
// wins; otherwise we fall back to the repo basename, then the cwd
// basename, so sessions still bucket sensibly without any explicit
// labeling.
func groupOf(s mdl.Session) string {
	if s.UserGroup != "" {
		return s.UserGroup
	}
	if s.Repo != "" {
		return s.Repo
	}
	if s.Cwd != "" {
		return filepath.Base(s.Cwd)
	}
	return ""
}

// uniqueGroups returns the labels available for the tab-strip cycle.
// User-named groups (sessions.user_group) always appear; the auto-derived
// repo / cwd names appear only when includeAutoRepo is true. The
// ""-at-front sentinel represents "All / no filter".
func (m *model) uniqueGroups() []string {
	user := map[string]struct{}{}
	auto := map[string]struct{}{}
	for _, s := range m.allSessions {
		if s.UserGroup != "" {
			user[s.UserGroup] = struct{}{}
			continue
		}
		if !m.settings.AutoRepoTabs {
			continue
		}
		switch {
		case s.Repo != "":
			auto[s.Repo] = struct{}{}
		case s.Cwd != "":
			auto[filepath.Base(s.Cwd)] = struct{}{}
		}
	}
	out := []string{""}
	for k := range user {
		out = append(out, k)
	}
	for k := range auto {
		if _, dup := user[k]; dup {
			continue
		}
		out = append(out, k)
	}
	tail := out[1:]
	for i := 0; i < len(tail); i++ {
		for j := i + 1; j < len(tail); j++ {
			if tail[j] < tail[i] {
				tail[i], tail[j] = tail[j], tail[i]
			}
		}
	}
	return out
}

func (m *model) cycleGroup(delta int) tea.Cmd {
	projects := m.uniqueGroups()
	if len(projects) <= 1 {
		m.flash = "no other projects to filter to"
		return nil
	}
	idx := 0
	for i, p := range projects {
		if p == m.groupFilter {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(projects)) % len(projects)
	m.groupFilter = projects[idx]
	m.applyGroupFilter()
	m.selSess = m.defaultSelectionIdx()
	m.sessScroll = 0
	m.tailScroll = 0
	m.tailPath = ""
	m.tailMtime = time.Time{}
	return m.loadTailCmd()
}

// defaultSelectionIdx returns the cursor position to land on when the
// session set has just changed (tab switch, archive view toggle, fresh
// startup). Newest is always preferable; with NewestAtBottom enabled
// that's the LAST entry in m.sessions.
func (m *model) defaultSelectionIdx() int {
	if len(m.sessions) == 0 {
		return 0
	}
	if m.settings.NewestAtBottom {
		return len(m.sessions) - 1
	}
	return 0
}

// mouseInRightPane decides whether a wheel event lands on the transcript
// pane (vs the session list) based on the active layout. We recompute the
// pane geometry on demand instead of caching it because View runs every
// frame and the terminal can resize between events.
func (m *model) mouseInRightPane(msg tea.MouseMsg) bool {
	headerH := countLines(m.renderHeader())
	tabH := 0
	if m.renderTabBar() != "" {
		tabH = 2
	}
	footerH := countLines(m.renderFooter()) + 1 // +1 for the spacer above the footer
	bodyHeight := m.height - headerH - tabH - footerH
	if bodyHeight < 5 {
		bodyHeight = 5
	}
	bodyTop := headerH + tabH
	if m.useVerticalLayout() {
		listH, _ := m.verticalSplit(bodyHeight)
		// +1 for the separator row between the list and the transcript.
		return msg.Y >= bodyTop+listH+1
	}
	leftW := m.width / 2
	if leftW < 30 {
		leftW = 30
	}
	return msg.X >= leftW+3 // 3-col vertical separator
}

// tailHalfPage approximates half the right pane's visible height. We don't
// know the exact pane size at key-handler time (it depends on the summary
// section, approval section, and header), so we use half the terminal
// height as a reasonable upper bound. The scroll is clamped at render
// time so over-shooting is harmless.
func (m *model) tailHalfPage() int {
	step := m.height / 2
	if step < 1 {
		step = 1
	}
	return step
}

type summaryDoneMsg struct {
	sessionID string
	summary   string
	err       error
}

// summarizeCurrent kicks off `claude -p` against the selected session's
// transcript. The DB row is flipped to summary_status='running' immediately
// so the list row shows the in-progress indicator on the next tick. The
// summary text comes back via a summaryDoneMsg which writes to the DB.
func (m *model) summarizeCurrent() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	if s.TranscriptPath == "" {
		m.flash = "no transcript path recorded for this session"
		return nil
	}
	sid := s.SessionID
	path := s.TranscriptPath
	// Flip status synchronously so the next refresh shows "summarizing".
	_ = m.db.SetSummaryStatus(m.ctx, sid, "running")
	secs := m.settings.SummaryTimeoutSec
	if secs <= 0 {
		secs = 180
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, time.Duration(secs)*time.Second)
		defer cancel()
		summary, err := summarize.Run(ctx, path)
		return summaryDoneMsg{sessionID: sid, summary: summary, err: err}
	}
}

// handleKeyTitleEdit consumes keystrokes while the rename input is active.
// We avoid the larger key map here so typed letters land in the buffer
// rather than triggering global shortcuts.
func (m *model) handleKeyTitleEdit(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		var cmd tea.Cmd
		if m.editingGroup {
			cmd = m.commitGroupEdit()
			m.editingGroup = false
		} else {
			cmd = m.commitTitleEdit()
			m.editingTitle = false
		}
		m.titleBuffer = ""
		return m, cmd
	case tea.KeyEsc, tea.KeyCtrlC:
		m.editingTitle = false
		m.editingGroup = false
		m.titleBuffer = ""
		return m, nil
	case tea.KeyUp:
		if m.editingGroup {
			m.pickTabCandidate(-1)
			return m, nil
		}
	case tea.KeyDown:
		if m.editingGroup {
			m.pickTabCandidate(1)
			return m, nil
		}
	case tea.KeyBackspace:
		runes := []rune(m.titleBuffer)
		if len(runes) > 0 {
			m.titleBuffer = string(runes[:len(runes)-1])
		}
		m.groupCandIdx = -1 // typing breaks the picker selection
	case tea.KeySpace:
		m.titleBuffer += " "
		m.groupCandIdx = -1
	case tea.KeyRunes:
		m.titleBuffer += string(msg.Runes)
		m.groupCandIdx = -1
	}
	return m, nil
}

// pickTabCandidate moves the candidate cursor by delta and copies the
// chosen value into the buffer. Wraps at both ends; idle (-1) goes to the
// first candidate on KeyDown / last on KeyUp so the operator can start
// browsing immediately on Tᴜ edit open.
func (m *model) pickTabCandidate(delta int) {
	cands := m.filteredGroupCandidates()
	if len(cands) == 0 {
		return
	}
	switch {
	case m.groupCandIdx < 0 && delta > 0:
		m.groupCandIdx = 0
	case m.groupCandIdx < 0 && delta < 0:
		m.groupCandIdx = len(cands) - 1
	default:
		m.groupCandIdx = (m.groupCandIdx + delta + len(cands)) % len(cands)
	}
	m.titleBuffer = cands[m.groupCandIdx]
}

// promptArchiveCurrentGroup prepares a bulk archive (or unarchive, when in
// the archive view) of every session that matches the current filter. We
// require an explicit tab — running it on the All bucket would torch the
// entire dashboard at once. The next keystroke confirms or cancels via
// the awaitGroupArchiveConfirm gate.
func (m *model) promptArchiveCurrentGroup() tea.Cmd {
	if m.groupFilter == "" {
		m.flash = "switch to a specific group first (h/l cycles)"
		return nil
	}
	if len(m.sessions) == 0 {
		m.flash = "group is empty"
		return nil
	}
	verb := "archive"
	if m.showArchived {
		verb = "unarchive"
	}
	m.awaitGroupArchiveConfirm = true
	m.flash = fmt.Sprintf("%s all %d sessions in '%s'? press 'y' to confirm", verb, len(m.sessions), m.groupFilter)
	return nil
}

// archiveCurrentGroup applies SetArchived to every session in m.sessions.
// In the archive view we reverse the action so this same shortcut also
// pulls a group back out of archive in one keystroke. The current group
// is about to go empty, so cycle to the next one synchronously — the
// operator shouldn't have to stare at a blank list while the DB writes
// settle.
func (m *model) archiveCurrentGroup() tea.Cmd {
	group := m.groupFilter
	want := !m.showArchived
	sids := make([]string, 0, len(m.sessions))
	for _, s := range m.sessions {
		sids = append(sids, s.SessionID)
	}
	verb := "archived"
	if !want {
		verb = "unarchived"
	}
	// Move forward in the cycle BEFORE the DB ops kick off so the next
	// render shows the new group populated. cycleGroup computes the
	// next entry from the still-current m.allSessions (which still
	// includes the soon-to-be-archived rows), then applyGroupFilter
	// scopes m.sessions to the new group so they aren't visible there.
	// We skip the cycle when --group pinned the operator: they
	// explicitly asked to stay on this group.
	var cycleCmd tea.Cmd
	if !m.groupLocked {
		cycleCmd = m.cycleGroup(1)
	}
	return tea.Batch(
		cycleCmd,
		func() tea.Msg {
			for _, sid := range sids {
				_ = m.db.SetArchived(m.ctx, sid, want)
			}
			return attachDoneMsg{msg: fmt.Sprintf("%s %d sessions in '%s'", verb, len(sids), group)}
		},
		m.refresh(),
	)
}

func (m *model) toggleArchiveCurrent() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	want := !s.Archived
	sid := s.SessionID
	verb := "archived"
	if !want {
		verb = "unarchived"
	}
	return func() tea.Msg {
		if err := m.db.SetArchived(m.ctx, sid, want); err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{msg: verb + " " + shortID(sid)}
	}
}

func (m *model) toggleFavoriteCurrent() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	want := !s.Favorite
	sid := s.SessionID
	verb := "favorited"
	if !want {
		verb = "unfavorited"
	}
	return func() tea.Msg {
		if err := m.db.SetFavorite(m.ctx, sid, want); err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{msg: verb + " " + shortID(sid)}
	}
}

func (m *model) startTitleEdit() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	m.editingTitle = true
	m.titleBuffer = s.CustomTitle
	if m.titleBuffer == "" {
		m.titleBuffer = s.Title
	}
	return nil
}

func (m *model) startGroupEdit() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	m.editingGroup = true
	m.titleBuffer = m.sessions[m.selSess].UserGroup
	m.groupCandIdx = -1
	return nil
}

// filteredGroupCandidates returns the unique user_group values currently
// in use across the unfiltered session set, narrowed by case-insensitive
// prefix match against the input buffer.
func (m *model) filteredGroupCandidates() []string {
	seen := map[string]struct{}{}
	var all []string
	for _, s := range m.allSessions {
		if s.UserGroup == "" {
			continue
		}
		if _, ok := seen[s.UserGroup]; ok {
			continue
		}
		seen[s.UserGroup] = struct{}{}
		all = append(all, s.UserGroup)
	}
	// Stable sort so the picker doesn't jiggle between renders.
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j] < all[i] {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	if m.titleBuffer == "" {
		return all
	}
	prefix := strings.ToLower(m.titleBuffer)
	out := all[:0]
	for _, c := range all {
		if strings.HasPrefix(strings.ToLower(c), prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (m *model) commitGroupEdit() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	sid := s.SessionID
	group := strings.TrimSpace(m.titleBuffer)
	return func() tea.Msg {
		if err := m.db.SetUserGroup(m.ctx, sid, group); err != nil {
			return attachDoneMsg{err: err}
		}
		if group == "" {
			return attachDoneMsg{msg: "cleared group for " + shortID(sid)}
		}
		return attachDoneMsg{msg: "group: " + group}
	}
}

func (m *model) commitTitleEdit() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	sid := s.SessionID
	title := strings.TrimSpace(m.titleBuffer)
	// If the buffer matches the auto-derived title we treat it as "clear
	// the override" so the auto title takes over again.
	if title == strings.TrimSpace(s.Title) {
		title = ""
	}
	return func() tea.Msg {
		if err := m.db.SetCustomTitle(m.ctx, sid, title); err != nil {
			return attachDoneMsg{err: err}
		}
		if title == "" {
			return attachDoneMsg{msg: "cleared custom title for " + shortID(sid)}
		}
		return attachDoneMsg{msg: "set title: " + shorten(title, 60)}
	}
}

// decideApproval sends the operator's allow/deny choice to the embedded
// server. With keep=true on an allow decision, the server adds an
// updatedPermissions block so Claude remembers the rule for the rest of
// the session — the next equivalent call won't pop a permission prompt.
func (m *model) decideApproval(behavior string, keep bool) tea.Cmd {
	pending := m.approvalsForSelected()
	if len(pending) == 0 {
		m.flash = "no pending approvals to " + behavior
		return nil
	}
	a := pending[0]
	id := a.ID
	tool := a.Tool
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]any{"behavior": behavior, "keep": keep})
		url := fmt.Sprintf("http://%s:%d/approvals/%d/decide", paths.DefaultHost, paths.DefaultPort, id)
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if tok, err := auth.Load(); err == nil {
			req.Header.Set(auth.HeaderName, tok)
		}
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return attachDoneMsg{err: err}
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			return attachDoneMsg{err: fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))}
		}
		verb := "allowed"
		if behavior == "deny" {
			verb = "denied"
		} else if keep {
			verb = "allowed (kept for session)"
		}
		return attachDoneMsg{msg: fmt.Sprintf("%s %s approval (#%d)", verb, tool, id)}
	}
}

func (m *model) handleKeyTranscript(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	bodyHeight := m.transcriptVisibleHeight()
	switch msg.String() {
	case "ctrl+c":
		return m, m.quit()
	case "q", "esc", "tab":
		m.pane = paneSessions
		return m, nil
	case "j", "down":
		m.transcriptScroll = clamp(m.transcriptScroll+1, 0, m.maxTranscriptScroll(bodyHeight))
	case "k", "up":
		m.transcriptScroll = clamp(m.transcriptScroll-1, 0, m.maxTranscriptScroll(bodyHeight))
	case "ctrl+d", "pgdown", " ":
		m.transcriptScroll = clamp(m.transcriptScroll+bodyHeight/2, 0, m.maxTranscriptScroll(bodyHeight))
	case "ctrl+u", "pgup":
		m.transcriptScroll = clamp(m.transcriptScroll-bodyHeight/2, 0, m.maxTranscriptScroll(bodyHeight))
	case "g", "home":
		m.transcriptScroll = 0
	case "G", "end":
		m.transcriptScroll = m.maxTranscriptScroll(bodyHeight)
	case "r":
		return m, m.reloadTranscript()
	}
	return m, nil
}

type transcriptLoadedMsg struct {
	path     string
	title    string
	messages []transcript.Message
	err      error
}

func (m *model) openTranscript() tea.Cmd {
	if m.pane != paneSessions || len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	if s.TranscriptPath == "" {
		m.flash = "no transcript path recorded for this session"
		return nil
	}
	path := s.TranscriptPath
	title := s.DisplayTitle()
	if title == "" {
		title = s.DisplayTitle()
	}
	if title == "" {
		title = shortID(s.SessionID)
	}
	return func() tea.Msg {
		msgs, err := transcript.Load(path)
		return transcriptLoadedMsg{path: path, title: title, messages: msgs, err: err}
	}
}

func (m *model) reloadTranscript() tea.Cmd {
	if m.transcriptPath == "" {
		return nil
	}
	path := m.transcriptPath
	title := m.transcriptTitle
	return func() tea.Msg {
		msgs, err := transcript.Load(path)
		return transcriptLoadedMsg{path: path, title: title, messages: msgs, err: err}
	}
}

type attachDoneMsg struct {
	err error
	msg string
}

// attachCurrent decides how to attach to the selected session and returns the
// appropriate command. It prefers switching to the existing tmux pane when we
// know one; otherwise falls back to `claude --resume <id>` when the session
// is fully stopped. If the session is running outside tmux, we cannot safely
// take it over, so we just show a flash message.
func (m *model) attachCurrent() tea.Cmd {
	if m.pane != paneSessions || len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	if s.Pane != "" {
		c := exec.Command("tmux", "switch-client", "-t", s.Pane)
		return tea.ExecProcess(c, func(err error) tea.Msg {
			return attachDoneMsg{err: err, msg: "switched to " + s.Pane}
		})
	}
	// No tmux pane — go through the inline attach path. We keep a Session
	// per session id so detaching with Ctrl+D leaves claude running in
	// the background; the next Enter on the same row resumes that same
	// child instead of spawning a fresh `claude --resume` (which would
	// fork the JSONL into a divergent copy). Sessions whose child has
	// died are pruned here so the next Enter starts cleanly.
	sess, ok := m.attached[s.SessionID]
	if ok && !sess.Alive() {
		_ = sess.Close()
		delete(m.attached, s.SessionID)
		sess = nil
		ok = false
	}
	if !ok {
		c := exec.Command("claude", "--resume", s.SessionID)
		if s.Cwd != "" {
			c.Dir = s.Cwd
		}
		sess = attach.New(c)
		m.attached[s.SessionID] = sess
	}
	ac := &attach.AttachCmd{Session: sess}
	sid := s.SessionID
	return tea.Exec(ac, func(err error) tea.Msg {
		if err != nil {
			return attachDoneMsg{err: err}
		}
		switch {
		case ac.Result.Detached:
			return attachDoneMsg{msg: "detached — claude still running in background (Enter to reattach)"}
		case ac.Result.ExitErr != nil:
			return attachDoneMsg{err: ac.Result.ExitErr, msg: "claude session ended (id " + shortID(sid) + ")"}
		default:
			return attachDoneMsg{msg: "claude session ended (id " + shortID(sid) + ")"}
		}
	})
}

// move shifts the selection. Returns a Cmd that refreshes the right pane
// (transcript tail) immediately for the new session, instead of waiting for
// the next tick.
func (m *model) move(delta int) tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	prev := m.selSess
	m.selSess = clamp(m.selSess+delta, 0, len(m.sessions)-1)
	if m.selSess != prev {
		m.tailPath = ""
		m.tailMtime = time.Time{}
		m.tailScroll = 0 // reset scroll when changing sessions
		return m.loadTailCmd()
	}
	return nil
}

func (m *model) jumpTo(idx int) tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	if idx < 0 {
		idx = len(m.sessions) - 1
	}
	m.selSess = clamp(idx, 0, len(m.sessions)-1)
	m.tailPath = ""
	m.tailMtime = time.Time{}
	m.tailScroll = 0
	return m.loadTailCmd()
}

func (m *model) View() string {
	if m.width == 0 {
		return "loading..."
	}
	header := m.renderHeader()
	tabBar := m.renderTabBar()
	footer := m.renderFooter()
	headerH := countLines(header)
	tabH := 0
	if tabBar != "" {
		// Tab strip + a blank "breathing room" line below it so the body
		// doesn't visually butt right up against the tabs.
		tabH = 2
	}
	// Match the tab strip's breathing-room line above the footer so the
	// help row doesn't feel pasted onto the body.
	const footerSpacer = 1
	footerH := countLines(footer) + footerSpacer
	bodyHeight := m.height - headerH - tabH - footerH
	if bodyHeight < 5 {
		bodyHeight = 5
	}
	body := clampLines(m.renderBody(bodyHeight), bodyHeight)
	parts := []string{header}
	if tabBar != "" {
		parts = append(parts, tabBar, "")
	}
	parts = append(parts, body, "", footer)
	out := lipgloss.JoinVertical(lipgloss.Left, parts...)
	// Final safety net: never emit more lines than the terminal can show, or
	// the alt-screen scrolls and the title bar disappears off the top.
	out = clampLines(out, m.height)
	if m.pendingBell {
		// Emit BEL inline so Bubble Tea writes it on the same channel as the
		// rest of the frame. Some terminals turn this into a visual flash;
		// most beep. We clear the flag so each transition rings only once.
		out = "\a" + out
		m.pendingBell = false
	}
	return out
}

// countLines returns the number of '\n'-separated lines in s, treating an
// empty string as zero lines.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// clampLines truncates s so it has at most n lines.
func clampLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if strings.Count(s, "\n")+1 <= n {
		return s
	}
	lines := strings.SplitN(s, "\n", n+1)
	return strings.Join(lines[:n], "\n")
}

var (
	titleStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	subtitleStyle     = lipgloss.NewStyle().Faint(true)
	// Black on bright orange — meant to be unmissable so a stale dev build
	// stands out against a release binary's clean header.
	devBadgeStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("208")).Padding(0, 1)
	selectedRow       = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("15"))
	pendingStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	pendingRowStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	groupHeaderStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("237"))
	tabActiveStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("12"))
	confirmBannerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("11")).Padding(0, 1)
	// Inactive tabs were on bg 236 + fg 8 — both grays, basically illegible.
	// Drop the background so inactive labels read against the terminal
	// default and bump the fg to 250 (light gray) for solid contrast.
	tabInactiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	approvalRowStyle   = lipgloss.NewStyle().Background(lipgloss.Color("58")).Foreground(lipgloss.Color("15"))
	approvalLabelStyle = lipgloss.NewStyle().Background(lipgloss.Color("11")).Foreground(lipgloss.Color("0")).Bold(true)
	statusActive      = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // bright green: busy
	statusIdle        = lipgloss.NewStyle().Foreground(lipgloss.Color("14")) // bright cyan: alive idle
	statusRecent      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))  // yellow: dead but <6h
	statusStop        = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))  // dim gray: long-dead
	footerStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	paneTitle         = lipgloss.NewStyle().Bold(true).Padding(0, 1).Background(lipgloss.Color("237")).Foreground(lipgloss.Color("15"))
	paneTitleDim      = lipgloss.NewStyle().Padding(0, 1).Foreground(lipgloss.Color("8"))
	errStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

func (m *model) renderHeader() string {
	left := titleStyle.Render("ccdash")
	pendingTotal := 0
	for _, s := range m.sessions {
		pendingTotal += s.PendingCount
	}
	pendingPart := subtitleStyle.Render(fmt.Sprintf("pending: %d", pendingTotal))
	if pendingTotal > 0 {
		// Plain bold-yellow text (no background/padding) so lipgloss.Width
		// matches the actual rendered width and the bar can't wrap to a
		// second visible line — that wrap was eating the tabs row.
		pendingPart = pendingStyle.Render(fmt.Sprintf("⚠ pending: %d", pendingTotal))
	}
	right := subtitleStyle.Render(fmt.Sprintf("sessions: %d  ", len(m.sessions))) +
		pendingPart +
		subtitleStyle.Render("  "+m.lastTick.Format("15:04:05"))
	if buildinfo.IsDev() {
		// On dev builds: bright orange "DEV", a content-derived hash so we
		// can spot stale binaries at a glance, and the binary's mtime as
		// the closest stand-in for "build time".
		parts := []string{"DEV"}
		if h := buildinfo.Hash(); h != "" {
			parts = append(parts, h)
		}
		if t := buildinfo.BuiltAt(); !t.IsZero() {
			parts = append(parts, t.Local().Format("2006-01-02 15:04"))
		}
		right += "  " + devBadgeStyle.Render(strings.Join(parts, " "))
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	leftLabel := titleStyle.Render("ccdash")
	if m.showArchived {
		leftLabel += " " + subtitleStyle.Render("[archive]")
	}
	if m.groupFilter != "" {
		leftLabel += " " + subtitleStyle.Render("· "+m.groupFilter)
	}
	if m.searchQuery != "" {
		leftLabel += " " + pendingStyle.Render("🔍 "+shorten(m.searchQuery, 30))
	}
	if m.groupLocked {
		leftLabel += " " + subtitleStyle.Render("(locked)")
	}
	if m.showArchived || m.groupFilter != "" || m.searchQuery != "" {
		left = leftLabel
		gap = m.width - lipgloss.Width(left) - lipgloss.Width(right)
		if gap < 1 {
			gap = 1
		}
	}
	bar := left + strings.Repeat(" ", gap) + right
	// Single-line rule under the title bar to separate header from body.
	// Count is computed from the rune's display width because '─' is East
	// Asian "ambiguous" and reports as 2 cols under CJK locales.
	charW := runewidth.RuneWidth('─')
	if charW < 1 {
		charW = 1
	}
	count := m.width / charW
	if count < 1 {
		count = 1
	}
	rule := subtitleStyle.Render(strings.Repeat("─", count))
	return lipgloss.JoinVertical(lipgloss.Left, bar, rule)
}

func (m *model) renderFooter() string {
	if m.awaitGroupArchiveConfirm || m.awaitSummaryConfirm || m.awaitMkdirConfirm {
		// y/n confirmation lands in a full-width yellow banner instead
		// of the dim flash so operators don't miss the cue.
		banner := confirmBannerStyle.Width(m.width).Render(m.flash)
		hint := footerStyle.Render("y confirm · any other key cancels")
		return banner + "\n" + hint
	}
	if m.editingSearch {
		prompt := "/" + m.titleBuffer + "▏"
		hint := subtitleStyle.Render("enter apply · esc cancel · empty=clear")
		return pendingStyle.Render(prompt) + "  " + hint
	}
	if m.editingNewSession {
		prompt := "new session in: " + m.newSessionBuffer + "▏"
		// Hint shifts by context: with a highlighted candidate Enter and
		// "/" both descend; without one Enter starts the session.
		var hint string
		if m.newSessionCompIdx >= 0 && m.newSessionCompIdx < len(m.newSessionCompletions) {
			hint = subtitleStyle.Render("tab/shift+tab cycle · enter or '/' descend · esc cancel")
		} else {
			hint = subtitleStyle.Render("tab cycle · enter start (creates dir if missing) · esc cancel")
		}
		// Always show live candidates so the operator doesn't have to hit
		// Tab to peek. Cap the visible count so a `~/` listing doesn't
		// drown the screen; the cap intentionally exceeds typical project
		// directory counts.
		const maxCands = 8
		cands := completeDirPrefix(m.newSessionBuffer)
		var candLine string
		if len(cands) == 0 {
			candLine = subtitleStyle.Render("(no matches)")
		} else {
			shown := cands
			suffix := ""
			if len(shown) > maxCands {
				suffix = subtitleStyle.Render(fmt.Sprintf("  …+%d more", len(shown)-maxCands))
				shown = shown[:maxCands]
			}
			labels := make([]string, len(shown))
			for i, c := range shown {
				display := shortenLeft(c, 40)
				if i == m.newSessionCompIdx {
					labels[i] = pendingStyle.Render("▶ " + display)
				} else {
					labels[i] = subtitleStyle.Render(display)
				}
			}
			candLine = strings.Join(labels, "  ") + suffix
		}
		return candLine + "\n" + pendingStyle.Render(prompt) + "  " + hint
	}
	if m.editingTitle {
		prompt := "rename: " + m.titleBuffer + "▏"
		hint := subtitleStyle.Render("enter save · esc cancel")
		return pendingStyle.Render(prompt) + "  " + hint
	}
	if m.editingGroup {
		prompt := "tab: " + m.titleBuffer + "▏"
		hint := subtitleStyle.Render("↑↓ pick · enter assign · esc cancel · empty=clear")
		cands := m.filteredGroupCandidates()
		if len(cands) == 0 {
			return pendingStyle.Render(prompt) + "  " + hint
		}
		labels := make([]string, len(cands))
		for i, c := range cands {
			if i == m.groupCandIdx {
				labels[i] = pendingStyle.Render("▶ " + c)
			} else {
				labels[i] = subtitleStyle.Render(c)
			}
		}
		candLine := subtitleStyle.Render("existing: ") + strings.Join(labels, "  ")
		return candLine + "\n" + pendingStyle.Render(prompt) + "  " + hint
	}
	keys := "↑/↓ sel  h/l tabs  / search  n new  enter attach  a/A/d allow/keep/deny  s sum  f fav  t/T rename/group  x/X arch  ctrl+x arch-group  o trans  , settings  q quit"
	if m.pane == paneSettings {
		keys = "↑/↓ select · space toggle · enter edit · esc back"
	}
	if m.showArchived {
		keys = "↑/↓ select  enter attach  x unarchive  X back to active  o transcript  q quit"
	}
	if m.pane == paneTranscript {
		keys = "↑/↓ scroll  pgup/pgdn page  g/G top/end  r reload  esc/q back"
	}
	if m.err != nil {
		return errStyle.Render("error: "+m.err.Error()) + "\n" + footerStyle.Render(keys)
	}
	if m.flash != "" {
		return subtitleStyle.Render(m.flash) + "\n" + footerStyle.Render(keys)
	}
	return footerStyle.Render(keys)
}

// renderTabBar lays out a browser-style strip of project / user-tab labels
// below the header, with the active filter highlighted. When the total
// width exceeds the terminal we center on the active label and surface
// ‹ / › arrows so the operator knows there's more off-screen.
func (m *model) renderTabBar() string {
	if m.pane != paneSessions {
		return ""
	}
	if m.groupLocked {
		// --tab pinned the operator to a single bucket; the strip would
		// be a one-button row that does nothing useful.
		return ""
	}
	tabs := m.uniqueGroups()
	if len(tabs) <= 1 {
		return ""
	}
	display := make([]string, len(tabs))
	activeIdx := 0
	for i, t := range tabs {
		label := t
		if label == "" {
			label = "All"
		}
		styled := " " + label + " "
		if t == m.groupFilter {
			display[i] = tabActiveStyle.Render(styled)
			activeIdx = i
		} else {
			display[i] = tabInactiveStyle.Render(styled)
		}
	}
	return slideTabs(display, activeIdx, m.width)
}

// slideTabs picks a window of the items list that fits in maxW, centered
// on the active index. Overflow on either side is announced with arrow
// markers; both sides reserve space even when not overflowing so the row
// width stays stable across selections.
func slideTabs(items []string, active, maxW int) string {
	widths := make([]int, len(items))
	for i, s := range items {
		widths[i] = lipgloss.Width(s)
	}
	leftMark := subtitleStyle.Render("‹ ")
	rightMark := subtitleStyle.Render(" ›")
	leftPad := strings.Repeat(" ", lipgloss.Width(leftMark))
	rightPad := strings.Repeat(" ", lipgloss.Width(rightMark))
	budget := maxW - lipgloss.Width(leftMark) - lipgloss.Width(rightMark)
	if budget < 1 {
		budget = 1
	}
	used := widths[active]
	lo, hi := active, active
	for {
		expanded := false
		if hi+1 < len(items) && used+widths[hi+1] <= budget {
			hi++
			used += widths[hi]
			expanded = true
		}
		if lo > 0 && used+widths[lo-1] <= budget {
			lo--
			used += widths[lo]
			expanded = true
		}
		if !expanded {
			break
		}
	}
	var b strings.Builder
	if lo > 0 {
		b.WriteString(leftMark)
	} else {
		b.WriteString(leftPad)
	}
	for i := lo; i <= hi; i++ {
		b.WriteString(items[i])
	}
	if hi < len(items)-1 {
		b.WriteString(rightMark)
	} else {
		b.WriteString(rightPad)
	}
	return b.String()
}

func (m *model) renderBody(height int) string {
	switch m.pane {
	case paneTranscript:
		return m.renderTranscriptBody(height)
	case paneSettings:
		return m.renderSettingsBody(height)
	default:
		return m.renderSessionsBody(height)
	}
}

func (m *model) handleKeySettings(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	specs := settings.AllSpecs()
	if m.settingsEdit {
		switch msg.Type {
		case tea.KeyEnter:
			cur := specs[m.settingsSel]
			n, err := strconv.Atoi(m.settingsBuffer)
			if err == nil {
				if cur.Min > 0 && n < cur.Min {
					n = cur.Min
				}
				if cur.Max > 0 && n > cur.Max {
					n = cur.Max
				}
				next, err := settings.Set(m.ctx, m.db, m.settings, cur.Key, n)
				if err == nil {
					m.settings = next
				} else {
					m.err = err
				}
			}
			m.settingsEdit = false
			m.settingsBuffer = ""
			return m, nil
		case tea.KeyEsc, tea.KeyCtrlC:
			m.settingsEdit = false
			m.settingsBuffer = ""
			return m, nil
		case tea.KeyBackspace:
			if r := []rune(m.settingsBuffer); len(r) > 0 {
				m.settingsBuffer = string(r[:len(r)-1])
			}
		case tea.KeyRunes:
			for _, r := range msg.Runes {
				if r >= '0' && r <= '9' {
					m.settingsBuffer += string(r)
				}
			}
		}
		return m, nil
	}
	switch msg.String() {
	case "esc", "q", ",":
		m.pane = paneSessions
		return m, nil
	case "j", "down":
		if m.settingsSel+1 < len(specs) {
			m.settingsSel++
		}
		return m, nil
	case "k", "up":
		if m.settingsSel > 0 {
			m.settingsSel--
		}
		return m, nil
	case "g", "home":
		m.settingsSel = 0
		return m, nil
	case "G", "end":
		m.settingsSel = len(specs) - 1
		return m, nil
	case " ", "enter":
		cur := specs[m.settingsSel]
		switch cur.Kind {
		case settings.KindBool:
			old := settings.Get(m.settings, cur.Key).(bool)
			next, err := settings.Set(m.ctx, m.db, m.settings, cur.Key, !old)
			if err == nil {
				m.settings = next
			} else {
				m.err = err
			}
		case settings.KindInt:
			m.settingsEdit = true
			v := settings.Get(m.settings, cur.Key).(int)
			m.settingsBuffer = strconv.Itoa(v)
		case settings.KindAction:
			if cur.Apply != nil {
				next, err := cur.Apply(m.ctx, m.db, m.settings)
				if err == nil {
					m.settings = next
					m.flash = "applied: " + cur.Label
				} else {
					m.err = err
				}
			}
		case settings.KindEnum:
			if len(cur.Options) > 0 {
				curVal, _ := settings.Get(m.settings, cur.Key).(string)
				idx := 0
				for i, o := range cur.Options {
					if o == curVal {
						idx = i
						break
					}
				}
				next := cur.Options[(idx+1)%len(cur.Options)]
				updated, err := settings.Set(m.ctx, m.db, m.settings, cur.Key, next)
				if err == nil {
					m.settings = updated
				} else {
					m.err = err
				}
			}
		}
	}
	return m, nil
}

func (m *model) renderSettingsBody(height int) string {
	specs := settings.AllSpecs()
	header := titleStyle.Render("settings") + "  " + subtitleStyle.Render("(persists across runs)")
	var rows []string
	for i, s := range specs {
		marker := "  "
		if i == m.settingsSel {
			marker = "▶ "
		}
		valStr := ""
		switch s.Kind {
		case settings.KindBool:
			// Render as "on · off" with the active option highlighted, so
			// every toggleable row reads the same way as the layout enum
			// below — operators don't have to context-switch between
			// "ON/OFF" labels and inline option lists.
			cur := settings.Get(m.settings, s.Key).(bool)
			activeIdx := 1
			if cur {
				activeIdx = 0
			}
			parts := []string{"on", "off"}
			rendered := make([]string, len(parts))
			for j, p := range parts {
				if j == activeIdx {
					rendered[j] = statusActive.Render(p)
				} else {
					rendered[j] = subtitleStyle.Render(p)
				}
			}
			valStr = strings.Join(rendered, " · ")
		case settings.KindInt:
			cur := settings.Get(m.settings, s.Key).(int)
			if m.settingsEdit && i == m.settingsSel {
				valStr = pendingStyle.Render(m.settingsBuffer + "▏")
			} else {
				valStr = fmt.Sprintf("%d", cur)
			}
			// Show the live terminal width next to the auto-vertical
			// threshold so the operator can pick a value relative to
			// their current window.
			if s.Key == settings.KeyVerticalAutoCols {
				marker := "≥ threshold"
				if m.width < cur {
					marker = "< threshold ⇒ vertical"
				}
				valStr += "  " + subtitleStyle.Render(fmt.Sprintf("(now: %d cols, %s)", m.width, marker))
			}
		case settings.KindAction:
			valStr = pendingStyle.Render("[run]")
		case settings.KindEnum:
			cur, _ := settings.Get(m.settings, s.Key).(string)
			parts := make([]string, 0, len(s.Options))
			for _, o := range s.Options {
				if o == cur {
					parts = append(parts, statusActive.Render(o))
				} else {
					parts = append(parts, subtitleStyle.Render(o))
				}
			}
			valStr = strings.Join(parts, " · ")
		}
		labelLine := fmt.Sprintf("%s%-32s  %s", marker, s.Label, valStr)
		if i == m.settingsSel {
			labelLine = selectedRow.Render(padRight(labelLine, m.width))
		}
		rows = append(rows, labelLine)
		rows = append(rows, subtitleStyle.Render("    "+s.Help))
		rows = append(rows, "")
	}
	hint := "↑/↓ select · space toggle / enter edit · esc back"
	body := strings.Join(rows, "\n")
	return lipgloss.JoinVertical(lipgloss.Left, header, "", body, subtitleStyle.Render(hint))
}

func (m *model) renderSessionsBody(height int) string {
	if m.useVerticalLayout() {
		return m.renderSessionsBodyVertical(height)
	}
	// Reserve 3 cols for " │ " separator.
	const sepW = 3
	leftWidth := m.width / 2
	if leftWidth < 30 {
		leftWidth = 30
	}
	rightWidth := m.width - leftWidth - sepW
	if rightWidth < 20 {
		rightWidth = 20
	}
	left := m.renderSessionsList(leftWidth, height)
	right := m.renderEventsList(rightWidth, height)
	// Build a vertical separator: " │ " repeated per row, joined with \n.
	sepLine := " " + subtitleStyle.Render("│") + " "
	sepLines := make([]string, height)
	for i := range sepLines {
		sepLines[i] = sepLine
	}
	sep := strings.Join(sepLines, "\n")
	return lipgloss.JoinHorizontal(lipgloss.Top, left, sep, right)
}

// useVerticalLayout returns true when the body should stack vertically.
// "auto" flips on narrow terminals so a 4K monitor split vertically into
// a tall column gets the stacked layout for free; "vertical" forces it
// regardless; "horizontal" never stacks even on narrow displays.
func (m *model) useVerticalLayout() bool {
	switch m.settings.LayoutMode {
	case "vertical":
		return true
	case "horizontal":
		return false
	default:
		threshold := m.settings.VerticalAutoCols
		if threshold <= 0 {
			threshold = 100
		}
		return m.width < threshold
	}
}

// renderSessionsBodyVertical stacks the list above the transcript pane.
// The split is 1/2 each minus a single separator line. Useful for narrow
// or tall terminals where horizontal width is the scarce resource.
func (m *model) renderSessionsBodyVertical(height int) string {
	listH, rightH := m.verticalSplit(height)
	list := m.renderSessionsList(m.width, listH)
	right := m.renderEventsList(m.width, rightH)
	// Use ASCII '-' rather than the box-drawing '─' so we don't get bitten
	// by terminals that disagree with runewidth on the East-Asian
	// "ambiguous" rendering. With '-' every cell is unambiguously one
	// column, so the rule reliably spans the full body width.
	sep := subtitleStyle.Render(strings.Repeat("-", m.width))
	return lipgloss.JoinVertical(lipgloss.Left, list, sep, right)
}

// verticalSplit returns the line allotments for the list and transcript
// panes in vertical layout. Extracted so the mouse handler can compute
// the same top/bottom boundary without re-rendering anything.
func (m *model) verticalSplit(height int) (listH, rightH int) {
	listH = height / 2
	if listH < 5 {
		listH = 5
	}
	rightH = height - listH - 1
	if rightH < 5 {
		rightH = 5
		listH = height - rightH - 1
		if listH < 5 {
			listH = 5
		}
	}
	return listH, rightH
}

func (m *model) renderSessionsList(width, height int) string {
	if len(m.sessions) == 0 {
		return lipgloss.NewStyle().Width(width).Height(height).
			Render(subtitleStyle.Render("no sessions yet"))
	}

	// Build a flat list of "rows" — header rows (1 line each) and session
	// rows (2 lines each) interleaved by date bucket. We render everything
	// to a single line slice and then pick a window that keeps the selected
	// session visible. This is simpler than tracking variable-height
	// blocks individually.
	type rowEntry struct {
		lines      []string
		sessionIdx int // -1 for headers
	}

	now := time.Now()
	var rows []rowEntry
	prevBucket := ""
	for i, s := range m.sessions {
		bucket := bucketFor(s, now)
		if bucket != prevBucket {
			label := bucket
			if bucket == bucketFavorites {
				label = "★ " + bucket
			}
			rows = append(rows, rowEntry{
				lines:      []string{groupHeaderStyle.Render(padRight(label, width))},
				sessionIdx: -1,
			})
			prevBucket = bucket
		}
		row := m.renderSessionRow(s, i == m.selSess, width)
		// renderSessionRow returns a 2-line block ending with "\n"; split.
		lines := strings.Split(strings.TrimRight(row, "\n"), "\n")
		rows = append(rows, rowEntry{lines: lines, sessionIdx: i})
	}

	// Flatten rows into a line slice and remember where each session lands.
	var allLines []string
	sessionLineStart := make([]int, len(m.sessions))
	for _, r := range rows {
		if r.sessionIdx >= 0 {
			sessionLineStart[r.sessionIdx] = len(allLines)
		}
		allLines = append(allLines, r.lines...)
	}

	// Reserve one line at the bottom for the "n/N" indicator.
	visibleH := height - 1
	if visibleH < 2 {
		visibleH = 2
	}

	// Adjust scroll so the selected session's lines (2 of them) stay in view.
	selStart := sessionLineStart[m.selSess]
	selEnd := selStart + 2 // session rows are always 2 lines
	if m.sessScroll > selStart {
		m.sessScroll = selStart
		// Pull in the preceding header if there is one and it fits.
		if m.sessScroll > 0 {
			m.sessScroll--
		}
	}
	if selEnd > m.sessScroll+visibleH {
		m.sessScroll = selEnd - visibleH
	}
	if m.sessScroll < 0 {
		m.sessScroll = 0
	}
	maxScroll := len(allLines) - visibleH
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.sessScroll > maxScroll {
		m.sessScroll = maxScroll
	}

	end := m.sessScroll + visibleH
	if end > len(allLines) {
		end = len(allLines)
	}

	body := strings.Join(allLines[m.sessScroll:end], "\n")
	indicator := fmt.Sprintf("%d / %d sessions", m.selSess+1, len(m.sessions))
	if m.sessScroll > 0 || end < len(allLines) {
		indicator += "  ↑↓ to scroll"
	}
	return lipgloss.NewStyle().Width(width).Height(height).Render(body + "\n" + subtitleStyle.Render(indicator))
}

const bucketFavorites = "Favorites"

// bucketFor returns the group label that a session belongs to. Favorites
// always go to the top regardless of date; everything else is bucketed by
// last_seen.
func bucketFor(s mdl.Session, now time.Time) string {
	if s.Favorite {
		return bucketFavorites
	}
	if s.LastSeen.IsZero() {
		return "Unknown"
	}
	t := s.LastSeen.Local()
	today := now.Local()
	if sameYMD(t, today) {
		return "Today"
	}
	if sameYMD(t, today.AddDate(0, 0, -1)) {
		return "Yesterday"
	}
	if t.After(today.AddDate(0, 0, -7)) {
		return "This week"
	}
	if t.After(today.AddDate(0, -1, 0)) {
		return "Earlier this month"
	}
	return t.Format("January 2006")
}

func sameYMD(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// renderSessionRow renders one session as a 2-line block:
//
//	▶ ● 1m   @task.md の内容から、具体的な作業内容を確認して
//	         ccmanage:main · a574854b · ⚠3
//
// Line 1 leads with status, age, and the title (the most useful identifier
// for the operator). Line 2 carries supporting metadata in dim text.
func (m *model) renderSessionRow(s mdl.Session, selected bool, width int) string {
	const indent = "         " // 9 columns, aligns with where title starts on line 1

	marker := " "
	if selected {
		marker = "▶"
	}
	statusDot := renderStatusDot(s.Status, m.animTick)
	age := runewidth.FillRight(humanDuration(time.Since(s.LastSeen)), 4)

	title := s.DisplayTitle()
	if title == "" {
		title = "(no prompt yet)"
	}
	if s.Favorite {
		title = "★ " + title
	}
	if s.PendingCount > 0 {
		title = "⚠ " + title
	}
	// Line 1 chrome: marker(1) + " "(1) + dot(1) + " "(1) + age(4) + " "(1) = 9 cols
	titleBudget := width - 9
	if titleBudget < 10 {
		titleBudget = 10
	}
	titleText := shorten(title, titleBudget)
	titleStyled := titleText
	switch {
	case selected:
		titleStyled = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Render(titleText)
	case s.PendingCount > 0:
		titleStyled = pendingRowStyle.Render(titleText)
	case s.Title == "":
		titleStyled = subtitleStyle.Render(titleText)
	}
	line1 := fmt.Sprintf("%s %s %s %s", marker, statusDot, subtitleStyle.Render(age), titleStyled)

	// Line 2: repo:branch · session-id · pending
	repo := baseLast(s.Cwd)
	if s.Branch != "" && s.Branch != "HEAD" {
		repo = repo + ":" + s.Branch
	}
	parts := []string{repo, shortID(s.SessionID)}
	if s.Pane != "" {
		parts = append(parts, "tmux:"+s.Pane)
	} else if s.ProcPID != 0 {
		parts = append(parts, fmt.Sprintf("pid:%d", s.ProcPID))
	}
	if s.PendingCount > 0 {
		parts = append(parts, pendingStyle.Render(fmt.Sprintf("⚠%d pending", s.PendingCount)))
	}
	switch s.SummaryStatus {
	case "running":
		parts = append(parts, pendingStyle.Render("⏳ summarizing"))
	case "error":
		parts = append(parts, statusStop.Render("✗ summary error"))
	}
	meta := strings.Join(parts, " · ")
	metaBudget := width - runewidth.StringWidth(indent)
	if metaBudget < 10 {
		metaBudget = 10
	}
	line2 := indent + subtitleStyle.Render(shorten(meta, metaBudget))

	if selected {
		line1 = selectedRow.Render(padRight(line1, width))
		line2 = selectedRow.Render(padRight(line2, width))
	}
	return line1 + "\n" + line2 + "\n"
}

func padRight(s string, width int) string {
	visible := lipgloss.Width(s)
	if visible >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visible)
}

// renderEventsList renders the right pane: a live transcript tail (with
// the cached summary inserted inline at its generation time) and a
// pending-approval section pinned to the bottom whenever the selected
// session has any approvals waiting.
func (m *model) renderEventsList(width, height int) string {
	if m.currentSessionID() == "" {
		return ""
	}

	header := subtitleStyle.Render(fmt.Sprintf("transcript  (%s)", shortID(m.currentSessionID())))

	approvals := m.approvalsForSelected()
	approvalSection := ""
	approvalH := 0
	if len(approvals) > 0 {
		approvalSection = m.renderApprovalSection(approvals, width)
		approvalH = strings.Count(approvalSection, "\n") + 1
		// Cap at half the pane so the transcript stays readable.
		if approvalH > height/2 {
			approvalH = height / 2
			approvalSection = clampLines(approvalSection, approvalH)
		}
	}

	transcriptH := height - 1 // header
	if approvalH > 0 {
		transcriptH -= approvalH
	}
	if transcriptH < 1 {
		transcriptH = 1
	}

	transcriptBody := m.renderTranscriptTail(width, transcriptH)
	out := header + "\n" + transcriptBody
	if approvalSection != "" {
		out += "\n" + approvalSection
	}
	return lipgloss.NewStyle().Width(width).Height(height).Render(out)
}

// renderSummaryBlock renders the summary as a transcript-flavored block:
// labelled row + body lines indented and styled as a system note. Returns
// nil when there's nothing to show. Used both for the chronological inline
// insertion and the running/error placeholders.
func (m *model) renderSummaryBlock(width int) []string {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	switch s.SummaryStatus {
	case "running":
		return []string{pendingStyle.Render("⏳ summary in progress…")}
	case "error":
		body := s.Summary
		if body == "" {
			body = "(no detail)"
		}
		return []string{
			statusStop.Render("✗ summary error"),
			subtitleStyle.Render("  " + shorten(body, width-3)),
		}
	case "done":
		// fall through to render
	default:
		return nil
	}
	if s.Summary == "" {
		return nil
	}
	age := summarize.SummaryAge(s.SummaryAt)
	header := titleStyle.Render("summary") + "  " + subtitleStyle.Render(age)
	bodyWidth := width - 2
	if bodyWidth < 20 {
		bodyWidth = 20
	}
	out := []string{header}
	for _, raw := range strings.Split(strings.TrimSpace(s.Summary), "\n") {
		for _, chunk := range wrapToWidth(raw, bodyWidth) {
			out = append(out, "  "+chunk)
		}
	}
	return out
}

// renderTranscriptTail builds the transcript line stream (including the
// inline summary) and returns the visible window for the given height,
// honoring the operator's tailScroll offset. The summary is inserted
// chronologically: the first transcript message whose timestamp is after
// SummaryAt pushes the summary block in just before it. New activity
// landing later naturally accumulates below the summary, so the summary
// scrolls up off-screen as the conversation continues — same way an old
// USER prompt would.
func (m *model) renderTranscriptTail(width, height int) string {
	if len(m.tailMessages) == 0 && m.summaryAvailable() == false {
		return subtitleStyle.Render("(no messages yet)")
	}
	bodyWidth := width - 1
	if bodyWidth < 20 {
		bodyWidth = 20
	}

	summaryBlock := m.renderSummaryBlock(bodyWidth)
	summaryAt := time.Time{}
	if len(m.sessions) > 0 {
		summaryAt = m.sessions[m.selSess].SummaryAt
	}

	var lines []string
	addBlock := func(block []string, leadBlank bool) {
		if len(block) == 0 {
			return
		}
		if leadBlank && len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, block...)
	}

	summaryInserted := summaryBlock == nil
	for i, msg := range m.tailMessages {
		// Insert the summary just before the first message that came
		// after generation. Running/error placeholders land at the
		// end of the buffer instead — they don't have a useful
		// SummaryAt yet.
		if !summaryInserted && !summaryAt.IsZero() && !msg.Timestamp.IsZero() && msg.Timestamp.After(summaryAt) {
			addBlock(summaryBlock, true)
			summaryInserted = true
		}
		// Keep a tool call and its result visually attached.
		leadBlank := i > 0 && msg.Kind != transcript.KindToolResult
		addBlock(renderTranscriptMessage(msg, bodyWidth), leadBlank)
	}
	if !summaryInserted {
		addBlock(summaryBlock, true)
	}

	if height < 1 {
		height = 1
	}
	// Clamp scroll: 0 = newest at bottom; max = top of buffer.
	maxScroll := len(lines) - height
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.tailScroll > maxScroll {
		m.tailScroll = maxScroll
	}
	end := len(lines) - m.tailScroll
	if end > len(lines) {
		end = len(lines)
	}
	if end < 1 {
		end = 1
	}
	start := end - height
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:end], "\n")
}

// summaryAvailable reports whether the selected session has anything to
// surface in the summary slot — used to decide between the "no messages
// yet" placeholder and rendering an empty list with just a summary.
func (m *model) summaryAvailable() bool {
	if len(m.sessions) == 0 {
		return false
	}
	s := m.sessions[m.selSess]
	return s.SummaryStatus != "" && (s.SummaryStatus != "done" || s.Summary != "")
}

// approvalsForSelected returns pending approvals for the currently selected
// session, in oldest-first order.
func (m *model) approvalsForSelected() []mdl.Approval {
	sid := m.currentSessionID()
	if sid == "" {
		return nil
	}
	out := make([]mdl.Approval, 0)
	for _, a := range m.approvals {
		if a.SessionID == sid {
			out = append(out, a)
		}
	}
	return out
}

// renderApprovalSection draws a compact "needs your decision" panel pinned
// to the bottom of the right pane. We use a yellow rule above so it's
// visually distinct from the transcript text.
func (m *model) renderApprovalSection(approvals []mdl.Approval, width int) string {
	if len(approvals) == 0 {
		return ""
	}
	charW := runewidth.RuneWidth('─')
	if charW < 1 {
		charW = 1
	}
	rule := pendingStyle.Render(strings.Repeat("─", width/charW))
	title := pendingStyle.Render(fmt.Sprintf("⚠ %d pending — 'a' allow · 'A' keep-allow · 'd' deny (oldest first)", len(approvals)))

	var blocks []string
	blocks = append(blocks, rule, title)
	bodyWidth := width - 4
	if bodyWidth < 10 {
		bodyWidth = 10
	}
	for _, a := range approvals {
		header := approvalLabelStyle.Render(fmt.Sprintf(" %s ", a.Tool)) +
			approvalRowStyle.Render(strings.Repeat(" ", maxInt(0, width-runewidth.StringWidth(" "+a.Tool+" "))))
		blocks = append(blocks, header)
		input := summarizeApprovalInput(a)
		for _, chunk := range wrapToWidth(input, bodyWidth) {
			content := "  " + chunk
			pad := width - runewidth.StringWidth(content)
			if pad < 0 {
				pad = 0
			}
			blocks = append(blocks, approvalRowStyle.Render(content+strings.Repeat(" ", pad)))
		}
	}
	return strings.Join(blocks, "\n")
}

// summarizeApprovalInput pulls a one-or-two-line summary out of the JSON
// blob the hook captured. Falls back to the raw JSON for tool kinds we
// don't have a special-case for.
func summarizeApprovalInput(a mdl.Approval) string {
	if len(a.ToolInput) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(a.ToolInput, &m); err != nil {
		return string(a.ToolInput)
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}
	switch a.Tool {
	case "Bash":
		return pick("command")
	case "Edit", "Write", "Read":
		return pick("file_path")
	case "Glob", "Grep":
		return pick("pattern", "query")
	case "WebFetch", "WebSearch":
		return pick("url", "query")
	}
	if s := pick("file_path", "command", "url", "query", "pattern"); s != "" {
		return s
	}
	return string(a.ToolInput)
}

func (m *model) unusedRenderApprovalsBody(height int) string {
	if len(m.approvals) == 0 {
		return lipgloss.NewStyle().Width(m.width).Height(height).
			Render(subtitleStyle.Render("no pending approvals"))
	}
	var b strings.Builder
	for i, a := range m.approvals {
		marker := "  "
		if i == m.selAppr {
			marker = "▶ "
		}
		row := fmt.Sprintf("%s%s  %-12s  %-7s  %s",
			marker,
			a.Timestamp.Local().Format("15:04:05"),
			shortID(a.SessionID),
			a.Tool,
			shorten(string(a.ToolInput), m.width-50),
		)
		if i == m.selAppr {
			row = selectedRow.Render(row)
		}
		b.WriteString(row + "\n")
	}
	return lipgloss.NewStyle().Width(m.width).Render(b.String())
}

func (m *model) transcriptVisibleHeight() int {
	h := m.height - 4 // header(2) + footer(1) + title bar(1)
	if h < 5 {
		h = 5
	}
	return h
}

func (m *model) renderTranscriptBody(height int) string {
	if len(m.transcriptMessages) == 0 {
		return lipgloss.NewStyle().Width(m.width).Height(height).Render(
			subtitleStyle.Render("(empty transcript)"))
	}
	rendered := m.renderedTranscriptLines()
	max := m.maxTranscriptScroll(height)
	if m.transcriptScroll > max {
		m.transcriptScroll = max
	}
	end := m.transcriptScroll + height
	if end > len(rendered) {
		end = len(rendered)
	}
	body := strings.Join(rendered[m.transcriptScroll:end], "\n")

	titleBar := titleStyle.Render(shorten(m.transcriptTitle, m.width-30)) + "  " +
		subtitleStyle.Render(fmt.Sprintf("%d-%d / %d lines", m.transcriptScroll+1, end, len(rendered)))

	return lipgloss.JoinVertical(lipgloss.Left, titleBar, body)
}

// renderedTranscriptLines flattens the parsed messages into wrapped display
// lines so scroll math works in line units.
func (m *model) renderedTranscriptLines() []string {
	width := m.width - 2
	if width < 30 {
		width = 30
	}
	var out []string
	for i, msg := range m.transcriptMessages {
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, renderTranscriptMessage(msg, width)...)
	}
	return out
}

func (m *model) maxTranscriptScroll(height int) int {
	total := len(m.renderedTranscriptLines())
	max := total - height
	if max < 0 {
		return 0
	}
	return max
}

// Per-role styles. Each row in the transcript pane is padded to the pane
// width and rendered with the background color so the color extends across
// the full row, making message boundaries obvious.
var (
	userRowStyle       = lipgloss.NewStyle().Background(lipgloss.Color("17")).Foreground(lipgloss.Color("15")) // dark blue
	userLabelStyle     = lipgloss.NewStyle().Background(lipgloss.Color("12")).Foreground(lipgloss.Color("15")).Bold(true)
	assistantRowStyle  = lipgloss.NewStyle().Background(lipgloss.Color("22")).Foreground(lipgloss.Color("15")) // dark green
	assistantLabelStyle = lipgloss.NewStyle().Background(lipgloss.Color("10")).Foreground(lipgloss.Color("0")).Bold(true)
	toolRowStyle       = lipgloss.NewStyle().Background(lipgloss.Color("23")).Foreground(lipgloss.Color("15")) // teal
	toolLabelStyle     = lipgloss.NewStyle().Background(lipgloss.Color("14")).Foreground(lipgloss.Color("0")).Bold(true)
	resultRowStyle     = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("7")) // dark gray
	resultLabelStyle   = lipgloss.NewStyle().Background(lipgloss.Color("242")).Foreground(lipgloss.Color("0")).Bold(true)
	errorRowStyle      = lipgloss.NewStyle().Background(lipgloss.Color("52")).Foreground(lipgloss.Color("15"))  // dark red
	errorLabelStyle    = lipgloss.NewStyle().Background(lipgloss.Color("9")).Foreground(lipgloss.Color("15")).Bold(true)
	thinkingRowStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
	thinkingLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true).Bold(true)
	systemRowStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	systemLabelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true)
)

func renderTranscriptMessage(msg transcript.Message, width int) []string {
	var label string
	var rowStyle, labelStyle lipgloss.Style
	body := msg.Text
	// labelIndent shifts the label-row to the right; bodyIndent does the same
	// for body rows. Tool results indent deeper so they read as "belonging"
	// to the tool_use just above them.
	labelIndent := ""
	bodyIndent := "  "

	switch msg.Kind {
	case transcript.KindUser:
		label, rowStyle, labelStyle = "USER", userRowStyle, userLabelStyle
	case transcript.KindAssistant:
		label, rowStyle, labelStyle = "CLAUDE", assistantRowStyle, assistantLabelStyle
	case transcript.KindThinking:
		label, rowStyle, labelStyle = "thinking", thinkingRowStyle, thinkingLabelStyle
	case transcript.KindToolUse:
		label, rowStyle, labelStyle = "TOOL "+msg.Tool, toolRowStyle, toolLabelStyle
		body = msg.ToolInput
	case transcript.KindToolResult:
		labelIndent = "  "
		bodyIndent = "      "
		if msg.IsError {
			label, rowStyle, labelStyle = "↳ ERROR", errorRowStyle, errorLabelStyle
		} else {
			label, rowStyle, labelStyle = "↳ result", resultRowStyle, resultLabelStyle
		}
	case transcript.KindSystem:
		label, rowStyle, labelStyle = "system", systemRowStyle, systemLabelStyle
	default:
		return nil
	}

	var out []string
	labelText := labelIndent + " " + label + " "
	pad := width - runewidth.StringWidth(labelText)
	if pad < 0 {
		pad = 0
	}
	// Render label-area background first so the indent on the left is also
	// drawn with the row's bg, then the bright label, then padding.
	labelRow := rowStyle.Render(labelIndent) + labelStyle.Render(" "+label+" ") + rowStyle.Render(strings.Repeat(" ", pad))
	out = append(out, labelRow)

	bodyWidth := width - runewidth.StringWidth(bodyIndent)
	if bodyWidth < 10 {
		bodyWidth = 10
	}
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return out
	}
	for _, raw := range strings.Split(body, "\n") {
		// Claude often emits trailing spaces on lines (markdown soft-break
		// convention) and sometimes \r before the \n on transcripts that
		// originated outside Unix. Trim every trailing Unicode whitespace
		// rune so the row's background color doesn't extend past the
		// actual content. \n is impossible here (we just split on it), so
		// IsSpace is the right cut.
		raw = strings.TrimRightFunc(raw, unicode.IsSpace)
		if raw == "" {
			out = append(out, rowStyle.Render(strings.Repeat(" ", width)))
			continue
		}
		for _, chunk := range wrapToWidth(raw, bodyWidth) {
			content := bodyIndent + chunk
			padN := width - runewidth.StringWidth(content)
			if padN < 0 {
				padN = 0
			}
			out = append(out, rowStyle.Render(content+strings.Repeat(" ", padN)))
		}
	}
	return out
}

// wrapToWidth splits s into chunks whose display width is at most width.
// We don't break on word boundaries — for chat content, hard wrapping reads
// fine and is simpler/safer with mixed-width characters.
func wrapToWidth(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	var out []string
	var line []rune
	var lineW int
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if lineW+rw > width && lineW > 0 {
			out = append(out, string(line))
			line = line[:0]
			lineW = 0
		}
		line = append(line, r)
		lineW += rw
	}
	if len(line) > 0 {
		out = append(out, string(line))
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}

// activeSpinnerFrames is the per-tick glyph cycle for "claude is working
// right now" rows. Braille cells render at single-cell width on every
// terminal worth supporting and the rotation is recognisable as motion
// even at small sizes.
var activeSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func renderStatusDot(s mdl.SessionStatus, tick int) string {
	switch s {
	case mdl.StatusActive:
		// Spin only on the active state — idle / recent / stopped get a
		// static dot so motion in the list reads as "this one is doing
		// something" and not visual noise.
		frame := activeSpinnerFrames[((tick%len(activeSpinnerFrames))+len(activeSpinnerFrames))%len(activeSpinnerFrames)]
		return statusActive.Bold(true).Render(frame)
	case mdl.StatusIdle:
		return statusIdle.Render("●")
	case mdl.StatusRecent:
		return statusRecent.Render("●")
	case mdl.StatusStopped:
		return statusStop.Render("●")
	}
	return "·"
}

func eventStyle(t mdl.EventType) lipgloss.Style {
	switch t {
	case mdl.EventPermissionRequest:
		return pendingStyle
	case mdl.EventPostToolFailure:
		return statusStop
	case mdl.EventUserPrompt:
		return titleStyle
	case mdl.EventPreTool, mdl.EventPostTool:
		return statusActive
	}
	return subtitleStyle
}

func pendingTag(n int) string {
	if n == 0 {
		return ""
	}
	return pendingStyle.Render(fmt.Sprintf("●%d", n))
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// shorten truncates s by display width, not rune count, so wide characters
// (Japanese, Chinese, emoji) don't cause line wraps even though their rune
// count looks fine.
func shorten(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if n <= 0 {
		return ""
	}
	return runewidth.Truncate(s, n, "…")
}

// shortenLeft truncates from the LEFT (so the trailing path segment, the
// part the operator usually cares about, stays visible). Inserts a leading
// ellipsis when truncation happened.
func shortenLeft(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= n {
		return s
	}
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		if runewidth.StringWidth("…"+string(r[i:])) <= n {
			return "…" + string(r[i:])
		}
	}
	return runewidth.Truncate(s, n, "…")
}

func baseLast(p string) string {
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	if len(parts) <= 2 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

func ttyForPID(pid int) string {
	for _, fd := range []string{"0", "1", "2"} {
		t, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", fd))
		if err != nil {
			continue
		}
		if strings.HasPrefix(t, "/dev/pts/") {
			return t
		}
	}
	return ""
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
