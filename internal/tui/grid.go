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
	"fmt"
	"math"
	"sort"
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

// Tile size floors: a tile narrower than gridMinTileW can't show a
// useful screen, and gridMinTileH is border + header + 2 lines.
const gridMinTileW = 34

// gridDims picks columns × rows for n tiles in a w × h body the way the
// portal's layoutGrid does: the column count giving each tile the most
// usable area (a sliver narrower than 0.6:1 or wider than 2.5:1 counts
// only up to that shape — a cell is about twice as tall as it is wide),
// discounted for empty cells.
func gridDims(n, w, h int) (cols, rows int) {
	if n <= 0 {
		return 0, 0
	}
	best, bestArea := 1, -1.0
	for c := 1; c <= n; c++ {
		r := (n + c - 1) / c
		tw, th := float64(w)/float64(c), float64(h)/float64(r)
		if tw < gridMinTileW && c > 1 {
			break
		}
		tv := th * 2 // height in width units
		area := math.Min(tw, tv*2.5) * math.Min(tv, tw/0.6) * math.Sqrt(float64(n)/float64(c*r))
		if area > bestArea {
			best, bestArea = c, area
		}
	}
	return best, (n + best - 1) / best
}

// gridCapacity is how many tiles fit a w × h body.
func gridCapacity(w, h int) int {
	return max(1, w/gridMinTileW) * max(1, h/gridMinTileH)
}

// gridRects lays n tiles out over a w × h body (body-relative cells).
// Rows and columns share the body evenly (leftover cells go to the first
// ones) and a part-filled last row stretches across, like the portal's —
// 5 tiles in 2 columns end with one wide tile, not a gap.
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
		inRow := min(cols, n-len(out))
		x := 0
		for c := 0; c < inRow; c++ {
			tw := w / inRow
			if c < w%inRow {
				tw++
			}
			out = append(out, rect{x, y, tw, th})
			x += tw
		}
		y += th
	}
	return out
}

// gridBelow finds the tile under (dir = 1) or over (dir = -1) tile i: in
// the nearest row that way, the tile whose middle is closest to i's.
// -1 when there is none.
func gridBelow(rects []rect, i, dir int) int {
	cur := rects[i]
	rowY := -1
	for _, r := range rects {
		if (r.y-cur.y)*dir > 0 && (rowY < 0 || (r.y-rowY)*dir < 0) {
			rowY = r.y
		}
	}
	if rowY < 0 {
		return -1
	}
	mid := 2*cur.x + cur.w // doubled to stay in integers
	best, bestD := -1, 0
	for j, r := range rects {
		if r.y != rowY {
			continue
		}
		d := 2*r.x + r.w - mid
		if d < 0 {
			d = -d
		}
		if best < 0 || d < bestD {
			best, bestD = j, d
		}
	}
	return best
}

// gridSessions returns the indices into m.sessions the grid shows: the
// sessions marked with space, else the ones worth watching (needs you,
// working, finished unseen) — capped to what fits.
func (m *model) gridSessions() []int {
	_, bodyH := m.bodyLayout()
	limit := gridCapacity(m.width, bodyH)
	var out []int
	for i, s := range m.sessions {
		if s.Grid {
			out = append(out, i)
		}
	}
	// In the order they were put on the grid (then #N), like the portal's.
	sort.SliceStable(out, func(a, b int) bool {
		x, y := m.sessions[out[a]], m.sessions[out[b]]
		if !x.GridAt.Equal(y.GridAt) {
			return x.GridAt.Before(y.GridAt)
		}
		return x.Num < y.Num
	})
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
		m.closeAllGridLive()
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

// toggleGridMark is space: put the selected session on (or off) the
// grid — the list shared with the portal, kept on the session row
// (Store.SetGrid). The row flips here at once; the write follows.
func (m *model) toggleGridMark() tea.Cmd {
	if len(m.sessions) == 0 || m.selSess < 0 || m.selSess >= len(m.sessions) {
		return nil
	}
	sid := m.sessions[m.selSess].SessionID
	on := !m.sessions[m.selSess].Grid
	at := time.Time{}
	if on {
		at = time.Now()
	}
	for _, list := range [][]mdl.Session{m.sessions, m.allSessions} {
		for i := range list {
			if list[i].SessionID == sid {
				list[i].Grid, list[i].GridAt = on, at
			}
		}
	}
	if on {
		m.flash = "on the grid (V shows it; the portal's grid too)"
	} else {
		m.flash = "off the grid"
	}
	if m.gridOpen && len(m.gridSessions()) == 0 {
		m.gridOpen = false
		m.closeAllGridLive()
	}
	st, ctx := m.store, m.ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := st.SetGrid(c, sid, on); err != nil {
			return errMsg{fmt.Errorf("grid: %w", err)}
		}
		return nil
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
	rects := gridRects(len(tiles), m.width, bodyH)
	cur := max(0, indexOf(tiles, m.selSess))
	next := cur
	switch msg.String() {
	case "esc", "V":
		return m.toggleGrid(), true
	case "enter":
		// A live tile: type into it in place.
		if l := m.focusedGridLive(); l != nil && l.client != nil && !l.exited {
			m.gridTyping = true
			return nil, true
		}
		// Else back to the dashboard on this session.
		m.gridOpen = false
		m.closeAllGridLive()
		if m.liveWantKey() != "" {
			m.liveFocus = true
		}
		return m.loadTailCmd(), true
	case "space":
		return m.toggleGridMark(), true
	case "tab", "right", "l":
		next = (cur + 1) % len(tiles)
	case "shift+tab", "left", "h":
		next = (cur - 1 + len(tiles)) % len(tiles)
	case "down", "j":
		if j := gridBelow(rects, cur, 1); j >= 0 {
			next = j
		}
	case "up", "k":
		if j := gridBelow(rects, cur, -1); j >= 0 {
			next = j
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

// clickGrid focuses the tile under a left click; a click on the focused
// live tile starts typing into it.
func (m *model) clickGrid(mm tea.Mouse) tea.Cmd {
	i, _, ok := m.gridTileAt(mm)
	if !ok {
		return nil
	}
	tiles := m.gridSessions()
	if m.selSess != tiles[i] {
		m.selSess = tiles[i]
		m.gridTyping = false
		return m.markSelectedSeen()
	}
	if l := m.focusedGridLive(); l != nil && l.client != nil && !l.exited {
		m.gridTyping = true
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
		if s.TranscriptPath == "" || m.gridLiveKey(s) != "" {
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

// renderGridTile is one tile, exactly w × h cells: a frame (the
// session's color when focused) whose top edge carries the working light,
// a header (status, #N, title, then what it's doing), and either the live
// screen of a session hosted here or the newest transcript lines.
func (m *model) renderGridTile(s mdl.Session, focused bool, w, h int) string {
	innerW := max(1, w-2)

	dot := renderStatusDot(s.Status, m.animTick)
	switch {
	case s.Attention == mdl.AttentionNeedsYou || s.PendingCount > 0:
		dot = pendingStyle.Bold(true).Render("?")
	case s.Attention == mdl.AttentionDone:
		dot = statusIdle.Bold(true).Render("✓")
	}
	head := " " + dot + " "
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
	titleStyle := lipgloss.NewStyle()
	if focused {
		titleStyle = titleStyle.Bold(true).Foreground(lipgloss.Color("15"))
	}

	var sub string
	switch {
	case s.PendingCount > 0 || (s.Attention == mdl.AttentionNeedsYou && s.AttentionReason != ""):
		reason := s.AttentionReason
		if s.PendingCount > 0 {
			reason = "approval pending — a allow · d deny"
		}
		sub = pendingStyle.Render("? " + reason)
	case workLine(s, time.Now()) != "":
		sub = statusActive.Render(workLine(s, time.Now()))
	default:
		where := baseLast(s.Cwd)
		if s.Branch != "" && s.Branch != "HEAD" {
			where += ":" + s.Branch
		}
		sub = subtitleStyle.Render(where + " · " + m.sessionTime(s.LastSeen))
	}

	live := m.gridLive[m.gridLiveKey(s)]
	if live == nil && m.gridLiveKey(s) != "" {
		if _, ok := tileScreen(rect{0, 0, w, h}); ok {
			live = m.livePlaceholder(m.gridLiveKey(s))
		}
	}
	typing := focused && m.gridTyping && live != nil

	var body []string
	if live != nil {
		// One header line: title, then the state, then the hint.
		hint := ""
		switch {
		case typing:
			hint = statusActive.Render("typing → claude · ctrl+] back")
		case live.exited:
			hint = statusStop.Render("ended")
		case focused:
			hint = subtitleStyle.Render("enter to type")
		}
		right := sub
		if hint != "" {
			right += subtitleStyle.Render(" · ") + hint
		}
		tw := max(4, innerW-ansi.StringWidth(head)-ansi.StringWidth(right)-2)
		body = append(body, head+titleStyle.Render(shorten(title, tw))+"  "+right)
		for y := 0; y < h-3; y++ {
			row := ""
			if y < len(live.rows) {
				row = live.rows[y]
			}
			body = append(body, row)
		}
	} else {
		body = append(body, head+titleStyle.Render(shorten(title, max(4, innerW-ansi.StringWidth(head)))), " "+sub)
		var tail []string
		if t := m.gridTails[s.SessionID]; t != nil {
			tail = tailLines(t.msgs, max(4, innerW-2))
		}
		if len(tail) == 0 {
			tail = []string{subtitleStyle.Render("(no messages yet)")}
		}
		if room := h - 2 - len(body); len(tail) > room {
			tail = tail[len(tail)-max(0, room):]
		}
		for _, l := range tail {
			body = append(body, " "+l)
		}
	}

	border := "240"
	if focused {
		border = "15"
		if c := s.Color(); c != "" {
			border = c
		}
	}
	if typing {
		border = "42"
	}
	glow := s.Status == mdl.StatusActive && m.settings.WorkAnim
	return tileFrame(body, w, h, lipgloss.Color(border), glow, m.animTick)
}

// tileFrame draws body inside a rounded frame of exactly w × h cells.
// Rows are padded / cut to the inner width, so live emulator rows go in
// as they are. With glow a light runs round the whole frame (beamCells;
// the portal beam's TUI cousin), drawn as heavy, colored border glyphs.
func tileFrame(body []string, w, h int, border lipgloss.Color, glow bool, tick int) string {
	inner := max(0, w-2)
	bs := lipgloss.NewStyle().Foreground(border)
	h = max(h, 2)

	// The frame's cells clockwise from the top-left corner: the top row,
	// down the right side, the bottom row backwards, up the left side.
	var lit map[int]string
	if glow {
		lit = beamCells(tick, 2*w+2*(h-2), gridLoopTicks)
	}
	glyph := func(p int, plain, heavy string) string {
		if c, ok := lit[p]; ok {
			return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Bold(true).Render(heavy)
		}
		return bs.Render(plain)
	}
	// edge renders a run of frame cells whose perimeter indexes come from at.
	edge := func(n int, at func(i int) int, plain, heavy string) string {
		var b strings.Builder
		run := 0
		for i := 0; i < n; i++ {
			if _, ok := lit[at(i)]; ok {
				if run > 0 {
					b.WriteString(bs.Render(strings.Repeat(plain, run)))
					run = 0
				}
				b.WriteString(glyph(at(i), plain, heavy))
				continue
			}
			run++
		}
		if run > 0 {
			b.WriteString(bs.Render(strings.Repeat(plain, run)))
		}
		return b.String()
	}
	right := func(y int) int { return w - 1 + y }                // y = 1 … h-2
	bottom := func(x int) int { return w + h - 2 + (w - 1 - x) } // x = 0 … w-1
	left := func(y int) int { return 2*w + h - 2 + (h - 2 - y) } // y = h-2 … 1

	lines := make([]string, 0, h)
	lines = append(lines, glyph(0, "╭", "┏")+edge(inner, func(i int) int { return 1 + i }, "─", "━")+glyph(w-1, "╮", "┓"))
	for y := 1; y <= h-2; y++ {
		row := ""
		if y-1 < len(body) {
			row = body[y-1]
		}
		lines = append(lines, glyph(left(y), "│", "┃")+fitWidth(row, inner)+glyph(right(y), "│", "┃"))
	}
	lines = append(lines, glyph(bottom(0), "╰", "┗")+edge(inner, func(i int) int { return bottom(1 + i) }, "─", "━")+glyph(bottom(w-1), "╯", "┛"))
	return strings.Join(lines, "\n")
}

func indexOf(xs []int, v int) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return -1
}
