package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/takumanakagame/ccmanage/internal/db"
	mdl "github.com/takumanakagame/ccmanage/internal/model"
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
	sessions   []mdl.Session
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
	cmds := []tea.Cmd{
		func() tea.Msg {
			ss, err := m.db.ListSessions(m.ctx)
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
		msgs, err := transcript.Load(path)
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
		m.sessions = []mdl.Session(msg)
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

// handleMouse routes wheel events to either the session selection or the
// transcript scroll, depending on which view is active. We don't bother with
// X/Y zoning yet — a single sensible action per pane is enough for MVP.
func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	const wheelStep = 3
	switch msg.Type {
	case tea.MouseWheelUp:
		if m.pane == paneTranscript {
			bodyHeight := m.transcriptVisibleHeight()
			m.transcriptScroll = clamp(m.transcriptScroll-wheelStep, 0, m.maxTranscriptScroll(bodyHeight))
			return m, nil
		}
		return m, m.move(-1)
	case tea.MouseWheelDown:
		if m.pane == paneTranscript {
			bodyHeight := m.transcriptVisibleHeight()
			m.transcriptScroll = clamp(m.transcriptScroll+wheelStep, 0, m.maxTranscriptScroll(bodyHeight))
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
	case "r":
		return m, m.refresh()
	case "enter":
		return m, m.attachCurrent()
	case "o":
		return m, m.openTranscript()
	}
	return m, nil
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
	title := s.Title
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
	keys := "↑/↓ select  enter attach  o transcript  r refresh  q quit"
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
	const rowsPerSession = 2
	// Reserve one line for the "n/N" indicator at the bottom.
	maxItems := (height - 1) / rowsPerSession
	if maxItems < 1 {
		maxItems = 1
	}
	// Scroll window so the selection is always visible. We track a sliding
	// window via m.sessScroll; clamp it on every render in case the underlying
	// list shrank between ticks.
	if m.selSess < m.sessScroll {
		m.sessScroll = m.selSess
	}
	if m.selSess >= m.sessScroll+maxItems {
		m.sessScroll = m.selSess - maxItems + 1
	}
	if m.sessScroll < 0 {
		m.sessScroll = 0
	}
	if m.sessScroll > maxInt(0, len(m.sessions)-maxItems) {
		m.sessScroll = maxInt(0, len(m.sessions)-maxItems)
	}

	end := m.sessScroll + maxItems
	if end > len(m.sessions) {
		end = len(m.sessions)
	}

	var b strings.Builder
	for i := m.sessScroll; i < end; i++ {
		s := m.sessions[i]
		selected := i == m.selSess
		b.WriteString(m.renderSessionRow(s, selected, width))
	}

	indicator := fmt.Sprintf("%d-%d / %d", m.sessScroll+1, end, len(m.sessions))
	if m.sessScroll > 0 || end < len(m.sessions) {
		indicator += "  ↑↓ to scroll"
	}
	b.WriteString(subtitleStyle.Render(indicator))
	// Constrain to the requested width AND height so JoinHorizontal with the
	// events pane doesn't accidentally make the row taller than `height`.
	return lipgloss.NewStyle().Width(width).Height(height).Render(b.String())
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

	title := s.Title
	if title == "" {
		title = "(no prompt yet)"
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
			// Don't insert a blank line between a tool call and its result —
			// keep them visually attached as one logical block.
			if msg.Kind != transcript.KindToolResult {
				lines = append(lines, "")
			}
		}
		lines = append(lines, renderTranscriptMessage(msg, bodyWidth)...)
	}
	if height < 1 {
		height = 1
	}
	start := len(lines) - height
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:], "\n")
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
	title := pendingStyle.Render(fmt.Sprintf("⚠ %d pending — switch to the claude session and respond", len(approvals)))

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
