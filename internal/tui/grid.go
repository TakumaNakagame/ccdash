package tui

// Grid view (V): several sessions side by side, each tile the tail of its
// transcript under a header with the working state — the TUI's take on the
// portal's grid. Tiles are read-only text, so nothing here resizes a PTY.
//
// The focused tile IS the selected session (m.selSess): moving between
// tiles moves the selection, so every per-session key (e, a/d, o, t, f,
// x, F …) works on the focused tile as is. The grid only takes the keys
// that move between tiles, plus enter / esc / V to leave it.

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

// gridTailBudget is how much of each tile's transcript is read; a tile
// shows a dozen lines or so, far less than the right pane.
const gridTailBudget = 64 * 1024

// gridMinTileH is the smallest tile: border (2) + header (2) + 2 lines.
const gridMinTileH = 6

type gridTail struct {
	path  string
	mtime time.Time
	msgs  []transcript.Message
}

type gridTailMsg struct {
	sid       string
	path      string
	mtime     time.Time
	msgs      []transcript.Message
	unchanged bool
}

type rect struct{ x, y, w, h int }

// gridDims picks columns × rows for n tiles in a w × h body.
func gridDims(n, w, h int) (cols, rows int) {
	if n <= 0 {
		return 0, 0
	}
	cols = 1
	switch {
	case w >= 180 && n >= 5:
		cols = 3
	case w >= 100 && n >= 2:
		cols = 2
	}
	if cols > n {
		cols = n
	}
	rows = (n + cols - 1) / cols
	return cols, rows
}

// gridCapacity is how many tiles fit a w × h body.
func gridCapacity(w, h int) int {
	cols, _ := gridDims(9, w, h)
	return cols * max(1, h/gridMinTileH)
}

// gridRects lays n tiles out over a w × h body (body-relative cells).
// Leftover columns / rows go to the first tiles so the grid fills the body.
func gridRects(n, w, h int) []rect {
	cols, rows := gridDims(n, w, h)
	if cols == 0 {
		return nil
	}
	out := make([]rect, 0, n)
	y := 0
	for r := 0; r < rows; r++ {
		th := h / rows
		if r < h%rows {
			th++
		}
		x := 0
		for c := 0; c < cols && len(out) < n; c++ {
			tw := w / cols
			if c < w%cols {
				tw++
			}
			out = append(out, rect{x, y, tw, th})
			x += tw
		}
		y += th
	}
	return out
}

// gridSessions returns the indices into m.sessions the grid shows: the
// sessions marked with space, else the ones worth watching (needs you,
// working, finished unseen) — capped to what fits.
func (m *model) gridSessions() []int {
	_, bodyH := m.bodyLayout()
	limit := gridCapacity(m.width, bodyH)
	var out []int
	for i, s := range m.sessions {
		if m.gridMarks[s.SessionID] {
			out = append(out, i)
		}
	}
	if len(out) == 0 {
		for i, s := range m.sessions {
			// The focused tile stays while the grid is open, even after
			// its session went quiet — the grid mustn't drop it under you.
			if s.Attention != "" || s.Status == mdl.StatusActive || s.Unseen || s.PendingCount > 0 ||
				(m.gridOpen && i == m.selSess) {
				out = append(out, i)
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// toggleGrid opens / closes the grid. Opening keeps the selection when
// it's one of the tiles, else focuses the first tile.
func (m *model) toggleGrid() tea.Cmd {
	if m.gridOpen {
		m.gridOpen = false
		return m.loadTailCmd()
	}
	tiles := m.gridSessions()
	if len(tiles) == 0 {
		m.flash = "nothing to show — mark sessions with space, or wait for one to work"
		return nil
	}
	m.gridOpen = true
	m.liveFocus = false
	if indexOf(tiles, m.selSess) < 0 {
		m.selSess = tiles[0]
	}
	return tea.Batch(m.gridLoadCmd(), m.markSelectedSeen())
}

// toggleGridMark is space: put the selected session in (or out of) the
// grid's chosen set.
func (m *model) toggleGridMark() {
	if len(m.sessions) == 0 || m.selSess < 0 || m.selSess >= len(m.sessions) {
		return
	}
	sid := m.sessions[m.selSess].SessionID
	if m.gridMarks == nil {
		m.gridMarks = map[string]bool{}
	}
	if m.gridMarks[sid] {
		delete(m.gridMarks, sid)
		m.flash = "removed from the grid"
	} else {
		m.gridMarks[sid] = true
		m.flash = "added to the grid (V shows it)"
	}
	if m.gridOpen && len(m.gridSessions()) == 0 {
		m.gridOpen = false
	}
}

// handleKeyGrid takes the keys that move between tiles and leave the
// grid; the rest fall through to the dashboard's keys (ok = false).
func (m *model) handleKeyGrid(msg tea.KeyPressMsg) (cmd tea.Cmd, ok bool) {
	tiles := m.gridSessions()
	if len(tiles) == 0 {
		m.gridOpen = false
		return nil, false
	}
	_, bodyH := m.bodyLayout()
	cols, _ := gridDims(len(tiles), m.width, bodyH)
	cur := max(0, indexOf(tiles, m.selSess))
	next := cur
	switch msg.String() {
	case "esc", "V":
		return m.toggleGrid(), true
	case "enter":
		// Back to the dashboard on this session — its live pane when it
		// runs in ccdash.
		m.gridOpen = false
		if m.liveWantKey() != "" {
			m.liveFocus = true
		}
		return m.loadTailCmd(), true
	case "space":
		m.toggleGridMark()
		return nil, true
	case "tab", "right", "l":
		next = (cur + 1) % len(tiles)
	case "shift+tab", "left", "h":
		next = (cur - 1 + len(tiles)) % len(tiles)
	case "down", "j":
		if cur+cols < len(tiles) {
			next = cur + cols
		}
	case "up", "k":
		if cur-cols >= 0 {
			next = cur - cols
		}
	default:
		return nil, false
	}
	if next != cur {
		m.selSess = tiles[next]
		return m.markSelectedSeen(), true
	}
	return nil, true
}

// clickGrid focuses the tile under a left click.
func (m *model) clickGrid(mm tea.Mouse) tea.Cmd {
	bodyTop, bodyH := m.bodyLayout()
	tiles := m.gridSessions()
	for i, r := range gridRects(len(tiles), m.width, bodyH) {
		if mm.X >= r.x && mm.X < r.x+r.w && mm.Y >= bodyTop+r.y && mm.Y < bodyTop+r.y+r.h {
			if m.selSess != tiles[i] {
				m.selSess = tiles[i]
				return m.markSelectedSeen()
			}
			return nil
		}
	}
	return nil
}

// gridLoadCmd (re)reads the tails of the tiles whose transcript moved.
func (m *model) gridLoadCmd() tea.Cmd {
	if !m.gridOpen {
		return nil
	}
	var cmds []tea.Cmd
	keep := map[string]bool{}
	for _, i := range m.gridSessions() {
		s := m.sessions[i]
		keep[s.SessionID] = true
		if s.TranscriptPath == "" {
			continue
		}
		prev := m.gridTails[s.SessionID]
		st, ctx := m.store, m.ctx
		cmds = append(cmds, func() tea.Msg {
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			mtime, _, err := st.TranscriptStat(c, s)
			if err != nil {
				return nil
			}
			if prev != nil && prev.path == s.TranscriptPath && !mtime.After(prev.mtime) {
				return gridTailMsg{sid: s.SessionID, unchanged: true}
			}
			tail, err := st.TranscriptTail(c, s, gridTailBudget)
			if err != nil {
				return nil
			}
			return gridTailMsg{sid: s.SessionID, path: s.TranscriptPath, mtime: tail.Mtime, msgs: tail.Messages}
		})
	}
	for sid := range m.gridTails {
		if !keep[sid] {
			delete(m.gridTails, sid)
		}
	}
	return tea.Batch(cmds...)
}

func (m *model) applyGridTail(msg gridTailMsg) {
	if msg.unchanged {
		return
	}
	if m.gridTails == nil {
		m.gridTails = map[string]*gridTail{}
	}
	m.gridTails[msg.sid] = &gridTail{path: msg.path, mtime: msg.mtime, msgs: msg.msgs}
}

// renderGridBody draws the tiles over the whole body.
func (m *model) renderGridBody(height int) string {
	tiles := m.gridSessions()
	rects := gridRects(len(tiles), m.width, height)
	canvas := make([]string, height)
	for i := range canvas {
		canvas[i] = strings.Repeat(" ", m.width)
	}
	for i, r := range rects {
		s := m.sessions[tiles[i]]
		box := m.renderGridTile(s, tiles[i] == m.selSess, r.w, r.h)
		for j, line := range strings.Split(box, "\n") {
			if r.y+j >= height {
				break
			}
			row := canvas[r.y+j]
			canvas[r.y+j] = ansi.Cut(row, 0, r.x) + line + ansi.Cut(row, r.x+ansi.StringWidth(line), m.width)
		}
	}
	return strings.Join(canvas, "\n")
}

// renderGridTile is one tile: a border (the session's color when
// focused), the row's header (status, #N, sweeping title), the work line
// or where it runs, then the newest transcript lines.
func (m *model) renderGridTile(s mdl.Session, focused bool, w, h int) string {
	innerW, innerH := max(4, w-4), max(1, h-2)

	dot := renderStatusDot(s.Status, m.animTick)
	switch {
	case s.Attention == mdl.AttentionNeedsYou || s.PendingCount > 0:
		dot = pendingStyle.Bold(true).Render("?")
	case s.Attention == mdl.AttentionDone:
		dot = statusIdle.Bold(true).Render("✓")
	}
	head := dot + " "
	if ref := s.Ref(); ref != "" {
		head += refStyle.Render(ref) + " "
	}
	if s.Unseen && !focused {
		head += newStyle.Render("NEW") + " "
	}
	title := s.DisplayTitle()
	if title == "" {
		title = "(no prompt yet)"
	}
	title = shorten(title, max(4, innerW-ansi.StringWidth(head)))
	titleStyle := lipgloss.NewStyle()
	if focused {
		titleStyle = titleStyle.Bold(true).Foreground(lipgloss.Color("15"))
	}
	if s.Status == mdl.StatusActive && m.settings.WorkAnim {
		head += sweep(title, m.animTick, titleStyle)
	} else {
		head += titleStyle.Render(title)
	}

	var sub string
	switch {
	case s.PendingCount > 0 || (s.Attention == mdl.AttentionNeedsYou && s.AttentionReason != ""):
		reason := s.AttentionReason
		if s.PendingCount > 0 {
			reason = "approval pending — a allow · d deny"
		}
		sub = pendingStyle.Render(shorten("? "+reason, innerW))
	case workLine(s, time.Now()) != "":
		sub = statusActive.Render(shorten(workLine(s, time.Now()), innerW))
	default:
		where := baseLast(s.Cwd)
		if s.Branch != "" && s.Branch != "HEAD" {
			where += ":" + s.Branch
		}
		sub = subtitleStyle.Render(shorten(where+" · "+m.sessionTime(s.LastSeen), innerW))
	}

	lines := []string{head, sub}
	if room := innerH - len(lines); room > 0 {
		var body []string
		if t := m.gridTails[s.SessionID]; t != nil {
			body = tailLines(t.msgs, innerW)
		}
		if len(body) == 0 {
			body = []string{subtitleStyle.Render("(no messages yet)")}
		}
		if len(body) > room {
			body = body[len(body)-room:]
		}
		lines = append(lines, body...)
	}
	for i, l := range lines {
		if wd := ansi.StringWidth(l); wd > innerW {
			lines[i] = ansi.Truncate(l, innerW, "")
		}
	}

	border := lipgloss.Color("240")
	if focused {
		border = lipgloss.Color("15")
		if c := s.Color(); c != "" {
			border = lipgloss.Color(c)
		}
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(w - 2).
		Height(h - 2).
		MaxHeight(h).
		Render(strings.Join(lines, "\n"))
}

func indexOf(xs []int, v int) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return -1
}
