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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/takumanakagame/ccmanage/internal/auth"
	"github.com/takumanakagame/ccmanage/internal/db"
	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/paths"
	"github.com/takumanakagame/ccmanage/internal/summarize"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

func Run(ctx context.Context, d *db.DB) error {
	m := newModel(ctx, d)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

type pane int

const (
	paneSessions pane = iota
	paneTranscript
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
	editingTab    bool
	titleBuffer   string
	projectFilter string // "" = All; otherwise repo / cwd basename

	// transcript view state
	transcriptMessages []transcript.Message
	transcriptScroll   int
	transcriptTitle    string
	transcriptPath     string
}

func newModel(ctx context.Context, d *db.DB) *model {
	return &model{ctx: ctx, db: d}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tickCmd())
}

type tickMsg time.Time

type sessionsMsg []mdl.Session
type eventsMsg []mdl.Event
type approvalsMsg []mdl.Approval
type errMsg struct{ err error }

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
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
		msgs, err := transcript.LoadTail(path, 256*1024)
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
		return m, tea.Batch(m.refresh(), tickCmd())
	case sessionsMsg:
		prev := m.currentSessionID()
		m.allSessions = []mdl.Session(msg)
		m.applyProjectFilter()
		// preserve selection by id when possible
		if prev != "" {
			for i, s := range m.sessions {
				if s.SessionID == prev {
					m.selSess = i
					break
				}
			}
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
		if m.bellPrimed && m.lastPendingTotal == 0 && total > 0 {
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
// always moves through that buffer. In the sessions view, the X coordinate
// of the wheel event decides whether we scroll the session list (left) or
// the transcript tail (right).
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
	leftW := m.width / 2
	if leftW < 30 {
		leftW = 30
	}
	inRight := msg.X >= leftW+3 // 3-col separator
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
	if m.editingTitle || m.editingTab {
		return m.handleKeyTitleEdit(msg)
	}
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
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
		return m, m.attachCurrent()
	case "o":
		return m, m.openTranscript()
	case "a":
		return m, m.decideApproval("allow", false)
	case "A":
		return m, m.decideApproval("allow", true)
	case "d":
		return m, m.decideApproval("deny", false)
	case "x":
		return m, m.toggleArchiveCurrent()
	case "X":
		m.showArchived = !m.showArchived
		m.selSess = 0
		m.tailScroll = 0
		return m, m.refresh()
	case "tab":
		return m, m.cycleProject(1)
	case "shift+tab":
		return m, m.cycleProject(-1)
	case "f":
		return m, m.toggleFavoriteCurrent()
	case "t":
		return m, m.startTitleEdit()
	case "T":
		return m, m.startTabEdit()
	case "s":
		return m, m.summarizeCurrent()
	}
	return m, nil
}

// applyProjectFilter recomputes m.sessions from m.allSessions using the
// current projectFilter ("" means show everything).
func (m *model) applyProjectFilter() {
	if m.projectFilter == "" {
		m.sessions = m.allSessions
		return
	}
	out := make([]mdl.Session, 0, len(m.allSessions))
	for _, s := range m.allSessions {
		if projectOf(s) == m.projectFilter {
			out = append(out, s)
		}
	}
	m.sessions = out
}

// projectOf names the group a session belongs to. Operator-set user_tab
// wins; otherwise we fall back to the repo basename, then the cwd
// basename, so sessions still bucket sensibly without any explicit
// labeling.
func projectOf(s mdl.Session) string {
	if s.UserTab != "" {
		return s.UserTab
	}
	if s.Repo != "" {
		return s.Repo
	}
	if s.Cwd != "" {
		return filepath.Base(s.Cwd)
	}
	return ""
}

// uniqueProjects returns the project labels present in the unfiltered set,
// with "" (= All) at the front.
func (m *model) uniqueProjects() []string {
	seen := map[string]struct{}{}
	for _, s := range m.allSessions {
		p := projectOf(s)
		if p != "" {
			seen[p] = struct{}{}
		}
	}
	out := []string{""}
	for k := range seen {
		out = append(out, k)
	}
	// Skip the "" sentinel when sorting.
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

func (m *model) cycleProject(delta int) tea.Cmd {
	projects := m.uniqueProjects()
	if len(projects) <= 1 {
		m.flash = "no other projects to filter to"
		return nil
	}
	idx := 0
	for i, p := range projects {
		if p == m.projectFilter {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(projects)) % len(projects)
	m.projectFilter = projects[idx]
	m.applyProjectFilter()
	m.selSess = 0
	m.sessScroll = 0
	m.tailScroll = 0
	m.tailPath = ""
	m.tailMtime = time.Time{}
	return m.loadTailCmd()
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
	return func() tea.Msg {
		// Bumped past the prior 90s because long sessions take Claude a
		// while to digest end-to-end; the previous timeout was killing
		// otherwise-fine summarizations and they'd land as 'error'.
		ctx, cancel := context.WithTimeout(m.ctx, 180*time.Second)
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
		if m.editingTab {
			cmd = m.commitTabEdit()
			m.editingTab = false
		} else {
			cmd = m.commitTitleEdit()
			m.editingTitle = false
		}
		m.titleBuffer = ""
		return m, cmd
	case tea.KeyEsc, tea.KeyCtrlC:
		m.editingTitle = false
		m.editingTab = false
		m.titleBuffer = ""
		return m, nil
	case tea.KeyBackspace:
		runes := []rune(m.titleBuffer)
		if len(runes) > 0 {
			m.titleBuffer = string(runes[:len(runes)-1])
		}
	case tea.KeySpace:
		m.titleBuffer += " "
	case tea.KeyRunes:
		m.titleBuffer += string(msg.Runes)
	}
	return m, nil
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

func (m *model) startTabEdit() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	m.editingTab = true
	m.titleBuffer = m.sessions[m.selSess].UserTab
	return nil
}

func (m *model) commitTabEdit() tea.Cmd {
	if len(m.sessions) == 0 {
		return nil
	}
	s := m.sessions[m.selSess]
	sid := s.SessionID
	tab := strings.TrimSpace(m.titleBuffer)
	return func() tea.Msg {
		if err := m.db.SetUserTab(m.ctx, sid, tab); err != nil {
			return attachDoneMsg{err: err}
		}
		if tab == "" {
			return attachDoneMsg{msg: "cleared tab for " + shortID(sid)}
		}
		return attachDoneMsg{msg: "tab: " + tab}
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
		return m, tea.Quit
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
	if s.ProcPID != 0 {
		tty := ttyForPID(s.ProcPID)
		return func() tea.Msg {
			info := fmt.Sprintf("running: pid=%d", s.ProcPID)
			if tty != "" {
				info += " tty=" + tty
			}
			info += " — switch to that terminal manually (or use tmux to enable Enter-to-attach)"
			return attachDoneMsg{msg: info}
		}
	}
	// Session is stopped — start a fresh `claude --resume`.
	c := exec.Command("claude", "--resume", s.SessionID)
	if s.Cwd != "" {
		c.Dir = s.Cwd
	}
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{msg: "claude session ended"}
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
	footer := m.renderFooter()
	headerH := countLines(header)
	footerH := countLines(footer)
	bodyHeight := m.height - headerH - footerH
	if bodyHeight < 5 {
		bodyHeight = 5
	}
	body := clampLines(m.renderBody(bodyHeight), bodyHeight)
	out := lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
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
	selectedRow       = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("15"))
	pendingStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	pendingRowStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	groupHeaderStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("237"))
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
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	leftLabel := titleStyle.Render("ccdash")
	if m.showArchived {
		leftLabel += " " + subtitleStyle.Render("[archive]")
	}
	if m.projectFilter != "" {
		leftLabel += " " + subtitleStyle.Render("· "+m.projectFilter)
	}
	if m.showArchived || m.projectFilter != "" {
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
	if m.editingTitle {
		prompt := "rename: " + m.titleBuffer + "▏"
		hint := subtitleStyle.Render("enter save · esc cancel")
		return pendingStyle.Render(prompt) + "  " + hint
	}
	if m.editingTab {
		prompt := "tab: " + m.titleBuffer + "▏"
		hint := subtitleStyle.Render("enter assign · esc cancel · empty=clear")
		return pendingStyle.Render(prompt) + "  " + hint
	}
	keys := "↑/↓ sel  tab tabs  J/K right-line  pgup/pgdn right-page  enter attach  a/A/d allow/keep/deny  s sum  f fav  t rename  T set-tab  x arch  X arch-view  o trans  q quit"
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

func (m *model) renderBody(height int) string {
	switch m.pane {
	case paneTranscript:
		return m.renderTranscriptBody(height)
	default:
		return m.renderSessionsBody(height)
	}
}

func (m *model) renderSessionsBody(height int) string {
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
	statusDot := renderStatusDot(s.Status)
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

// renderEventsList renders a live tail of the selected session's transcript
// — the actual user prompts, Claude responses, and tool exchanges that make
// up the conversation. We auto-scroll so the most recent exchange is at the
// bottom.
// renderEventsList renders the right pane: a live transcript tail at the
// top, plus a pending-approval section pinned to the bottom whenever the
// selected session has any approvals waiting.
func (m *model) renderEventsList(width, height int) string {
	if m.currentSessionID() == "" {
		return ""
	}

	header := subtitleStyle.Render(fmt.Sprintf("transcript  (%s)", shortID(m.currentSessionID())))

	summarySection := m.renderSummarySection(width)
	summaryH := 0
	if summarySection != "" {
		summaryH = strings.Count(summarySection, "\n") + 1
	}

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
	if summaryH > 0 {
		transcriptH -= summaryH
	}
	if approvalH > 0 {
		transcriptH -= approvalH
	}
	if transcriptH < 1 {
		transcriptH = 1
	}

	transcriptBody := m.renderTranscriptTail(width, transcriptH)
	out := header
	if summarySection != "" {
		out += "\n" + summarySection
	}
	out += "\n" + transcriptBody
	if approvalSection != "" {
		out += "\n" + approvalSection
	}
	return lipgloss.NewStyle().Width(width).Height(height).Render(out)
}

// renderSummarySection draws the cached LLM summary at the top of the right
// pane. We cap to ~6 lines so it doesn't crowd out the transcript; the full
// text is reachable by re-running 's' which shows it in the flash, or by
// reading the DB directly.
func (m *model) renderSummarySection(width int) string {
	if len(m.sessions) == 0 {
		return ""
	}
	s := m.sessions[m.selSess]
	switch s.SummaryStatus {
	case "running":
		title := pendingStyle.Render("⏳ summary in progress…")
		return title
	case "error":
		title := statusStop.Render("✗ summary error")
		body := subtitleStyle.Render("  " + shorten(s.Summary, width-3))
		return title + "\n" + body
	case "done":
		// fall through to render
	default:
		return ""
	}
	if s.Summary == "" {
		return ""
	}
	age := summarize.SummaryAge(s.SummaryAt)
	header := titleStyle.Render("summary") + "  " + subtitleStyle.Render(age)
	const maxLines = 6
	bodyWidth := width - 2
	if bodyWidth < 20 {
		bodyWidth = 20
	}
	var bodyLines []string
	for _, raw := range strings.Split(strings.TrimSpace(s.Summary), "\n") {
		for _, chunk := range wrapToWidth(raw, bodyWidth) {
			bodyLines = append(bodyLines, "  "+chunk)
			if len(bodyLines) >= maxLines {
				break
			}
		}
		if len(bodyLines) >= maxLines {
			break
		}
	}
	return header + "\n" + strings.Join(bodyLines, "\n")
}

func (m *model) renderTranscriptTail(width, height int) string {
	if len(m.tailMessages) == 0 {
		return subtitleStyle.Render("(no messages yet)")
	}
	bodyWidth := width - 1
	if bodyWidth < 20 {
		bodyWidth = 20
	}
	var lines []string
	for i, msg := range m.tailMessages {
		if i > 0 {
			// Keep a tool call and its result visually attached.
			if msg.Kind != transcript.KindToolResult {
				lines = append(lines, "")
			}
		}
		lines = append(lines, renderTranscriptMessage(msg, bodyWidth)...)
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

func renderStatusDot(s mdl.SessionStatus) string {
	switch s {
	case mdl.StatusActive:
		return statusActive.Render("●")
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
