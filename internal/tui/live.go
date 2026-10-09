package tui

// Live right pane: a virtual terminal view of a claude session that the
// ccdash server hosts in a PTY. The server runs the emulator (see
// internal/server/screen.go); this file is the viewer side — it subscribes
// to rendered rows over the screen stream, splices them into the right
// pane, forwards keys/paste/wheel while the pane has focus, and places the
// terminal's REAL cursor at the emulator's cursor so OS-level IMEs anchor
// their pre-edit overlay in the right spot.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/takumanakagame/ccmanage/internal/auth"
	"github.com/takumanakagame/ccmanage/internal/paths"
	"github.com/takumanakagame/ccmanage/internal/screen"
)

// liveScreen is the TUI-side mirror of one server emulator.
type liveScreen struct {
	key    string
	client *screen.Client
	rows   []string // one ANSI row per screen line, exactly w cols wide
	w, h   int
	cur    screen.Cursor
	// scroll is how far the server has this view scrolled back into the
	// scrollback (wheel on a child that doesn't track the mouse).
	scroll int
	// exited is set when the child died; the last screen stays visible
	// with a note until the server drops the entry.
	exited  bool
	exitErr string
	// sentCols/sentRows track the last geometry pushed to the server so we
	// only resize on change.
	sentCols, sentRows int
	// focusSent mirrors what the child was last told about focus.
	focusSent bool
	// connecting marks a placeholder rendered while the stream dials.
	connecting bool
}

type liveConnectedMsg struct {
	key    string
	client *screen.Client
	err    error
}

type liveFramesMsg struct {
	key    string
	frames []screen.Frame
}

// ptyListMsg carries GET /pty: which ptyKeys currently have a live child.
type ptyListMsg struct {
	alive map[string]bool
	pids  map[string]int // ptyKey → shell pid; aliases of one PTY share it
	err   error
}

// fetchPTYListCmd polls the server for live PTY keys. Cheap loopback call;
// runs on every refresh tick alongside the DB reads.
func fetchPTYListCmd() tea.Cmd {
	return func() tea.Msg {
		addr := fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort)
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/pty/", nil)
		if err != nil {
			return ptyListMsg{err: err}
		}
		if tok, err := auth.Load(); err == nil {
			req.Header.Set(auth.HeaderName, tok)
		}
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
		if err != nil {
			return ptyListMsg{err: err}
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return ptyListMsg{err: fmt.Errorf("pty list: %s", resp.Status)}
		}
		var rows []struct {
			Key   string `json:"key"`
			Alive bool   `json:"alive"`
			PID   int    `json:"pid"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
			return ptyListMsg{err: err}
		}
		alive := make(map[string]bool, len(rows))
		pids := make(map[string]int, len(rows))
		for _, r := range rows {
			if r.Alive {
				alive[r.Key] = true
				pids[r.Key] = r.PID
			}
		}
		return ptyListMsg{alive: alive, pids: pids}
	}
}

// paneGeom is a rectangle in terminal cells.
type paneGeom struct{ x, y, w, h int }

// bodyLayout returns the vertical arithmetic View() uses: where the body
// starts and how tall it is. Every geometry consumer (mouse zoning, live
// cursor placement, emulator sizing) must go through this so they agree
// with the renderer to the cell.
func (m *model) bodyLayout() (bodyTop, bodyHeight int) {
	headerH := countLines(m.renderHeader())
	tabH := 0
	if m.renderTabBar() != "" {
		tabH = 2
	}
	footerH := countLines(m.renderFooter()) + 1 // +1 spacer above the footer
	bodyHeight = m.height - headerH - tabH - footerH
	if bodyHeight < 5 {
		bodyHeight = 5
	}
	return headerH + tabH, bodyHeight
}

// rightPaneGeom is the whole right pane (its 1-line header included).
// ok is false when the sessions view isn't showing a right pane.
func (m *model) rightPaneGeom() (g paneGeom, ok bool) {
	if m.pane != paneSessions || m.width == 0 || m.currentSessionID() == "" {
		return paneGeom{}, false
	}
	bodyTop, bodyHeight := m.bodyLayout()
	if m.useVerticalLayout() {
		listH, rightH := m.verticalSplit(bodyHeight)
		return paneGeom{x: 0, y: bodyTop + listH + 1, w: m.width, h: rightH}, true
	}
	leftW := m.leftPaneWidth()
	rightW := m.width - leftW - 3
	if rightW < 20 {
		rightW = 20
	}
	return paneGeom{x: leftW + 3, y: bodyTop, w: rightW, h: bodyHeight}, true
}

// liveScreenGeom is the sub-rectangle of the right pane the emulator
// occupies: below the pane header, above any pinned approval section.
func (m *model) liveScreenGeom() (g paneGeom, ok bool) {
	p, ok := m.rightPaneGeom()
	if !ok {
		return paneGeom{}, false
	}
	_, approvalH := m.approvalBlock(p.w, p.h)
	h := p.h - 1 - approvalH
	if h < 1 {
		h = 1
	}
	return paneGeom{x: p.x, y: p.y + 1, w: p.w, h: h}, true
}

// desiredLiveKey names the ptyKey whose screen should fill the right pane
// for the selected session, or "" when the session isn't hosted here.
func (m *model) desiredLiveKey() string {
	if len(m.sessions) == 0 || m.selSess < 0 || m.selSess >= len(m.sessions) {
		return ""
	}
	s := m.sessions[m.selSess]
	if s.SessionID != "" && m.ptyAlive[s.SessionID] {
		return s.SessionID
	}
	// Freshly spawned sessions are keyed by PID until discovery resolves
	// the id and promotePTYKeys registers the alias.
	if s.ProcPID != 0 {
		if k := fmt.Sprintf("pid-%d", s.ProcPID); m.ptyAlive[k] {
			return k
		}
		if k, ok := m.pendingPTYKeys[s.ProcPID]; ok && m.ptyAlive[k] {
			return k
		}
	}
	return ""
}

// liveWantKey is the ptyKey the right pane should be streaming right now,
// or "" when it should show the transcript instead.
func (m *model) liveWantKey() string {
	// Remote mode talks to a collector on another host; its PTYs (if any)
	// would need the remote address + token plumbed through the Store,
	// which isn't wired yet. Keep the pane on the transcript there.
	if m.pane == paneSessions && m.settings.AttachEnabled && !m.remote.Enabled {
		return m.desiredLiveKey()
	}
	return ""
}

// livePlaceholder is what the right pane shows for key before its stream
// delivers a frame: the last screen we saw for it, or a blank one.
func (m *model) livePlaceholder(key string) *liveScreen {
	if c, ok := m.liveCache[key]; ok {
		p := *c
		p.client = nil
		p.connecting = true
		return &p
	}
	return &liveScreen{key: key, connecting: true}
}

// liveForCurrent returns the live screen if it belongs to the selected
// session, else nil.
func (m *model) liveForCurrent() *liveScreen {
	if m.live == nil || m.live.key != m.desiredLiveKey() {
		return nil
	}
	return m.live
}

// syncLive reconciles the live stream with the selection and pane
// geometry. Called after every Update so selection changes, resizes, and
// PTY list refreshes all funnel through one place.
func (m *model) syncLive() tea.Cmd {
	want := m.liveWantKey()
	if m.live != nil && m.live.key != want {
		m.closeLive()
	}
	if want == "" {
		m.liveFocus = false
		return nil
	}
	g, ok := m.liveScreenGeom()
	if !ok {
		return nil
	}
	if m.live == nil {
		if m.liveConnecting == want {
			return nil
		}
		m.liveConnecting = want
		return m.connectLiveCmd(want, g.w, g.h)
	}
	if m.live.client == nil || m.live.exited {
		return nil
	}
	if m.live.sentCols != g.w || m.live.sentRows != g.h {
		m.live.sentCols, m.live.sentRows = g.w, g.h
		if err := m.live.client.Resize(g.w, g.h); err != nil {
			m.err = err
		}
	}
	if m.live.focusSent != m.liveFocus {
		m.live.focusSent = m.liveFocus
		_ = m.live.client.Focus(m.liveFocus)
	}
	return nil
}

func (m *model) connectLiveCmd(key string, cols, rows int) tea.Cmd {
	return func() tea.Msg {
		addr := fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort)
		tok, _ := auth.Load()
		c, err := screen.Dial(addr, key, tok, cols, rows)
		return liveConnectedMsg{key: key, client: c, err: err}
	}
}

// waitLiveCmd blocks on the client's frame channel and delivers whatever
// has accumulated as one message so a burst of diffs applies in a single
// Update instead of one render per frame.
func waitLiveCmd(c *screen.Client) tea.Cmd {
	return func() tea.Msg {
		f, ok := <-c.Frames
		if !ok {
			return liveFramesMsg{key: c.Key, frames: []screen.Frame{{Type: screen.FrameTypeClosed}}}
		}
		frames := []screen.Frame{f}
		for {
			select {
			case f2, ok := <-c.Frames:
				if !ok {
					return liveFramesMsg{key: c.Key, frames: frames}
				}
				frames = append(frames, f2)
			default:
				return liveFramesMsg{key: c.Key, frames: frames}
			}
		}
	}
}

func (m *model) handleLiveConnected(msg liveConnectedMsg) tea.Cmd {
	if m.liveConnecting == msg.key {
		m.liveConnecting = ""
	}
	if msg.err != nil {
		// Don't hammer a key the server rejected; the next /pty poll
		// re-adds it if it comes back.
		delete(m.ptyAlive, msg.key)
		if m.live != nil && m.live.key == msg.key {
			m.closeLive()
		}
		m.flash = "live screen unavailable: " + msg.err.Error()
		return nil
	}
	if msg.key != m.desiredLiveKey() || m.live != nil {
		// Selection moved while we were dialing.
		msg.client.Close()
		return nil
	}
	g, _ := m.liveScreenGeom()
	m.live = &liveScreen{
		key:      msg.key,
		client:   msg.client,
		sentCols: g.w,
		sentRows: g.h,
	}
	// Show the last known screen until the server's first (full) frame
	// replaces it, so the pane never goes blank in between.
	if c, ok := m.liveCache[msg.key]; ok {
		m.live.rows = append([]string(nil), c.rows...)
		m.live.w, m.live.h, m.live.cur = c.w, c.h, c.cur
	}
	if m.liveFocusPending == msg.key {
		m.liveFocus = true
	}
	m.liveFocusPending = ""
	return waitLiveCmd(msg.client)
}

func (m *model) handleLiveFrames(msg liveFramesMsg) tea.Cmd {
	if m.live == nil || m.live.key != msg.key {
		return nil // stale stream
	}
	live := m.live
	for _, f := range msg.frames {
		switch f.Type {
		case screen.FrameTypeClosed:
			// Stream dropped (server restarted?). Forget the key so we
			// don't spin; the poll brings it back if it's still alive.
			delete(m.ptyAlive, msg.key)
			m.closeLive()
			if f.Err != "" {
				m.flash = "live screen closed: " + f.Err
			}
			return nil
		case screen.FrameTypeExit:
			live.exited = true
			live.exitErr = f.Err
			m.liveFocus = false
			if f.Err != "" {
				m.flash = "claude session ended: " + f.Err
			} else {
				m.flash = "claude session ended"
			}
			live.applyFrame(f)
		default:
			live.applyFrame(f)
		}
	}
	if live.exited {
		return nil
	}
	return waitLiveCmd(live.client)
}

func (l *liveScreen) applyFrame(f screen.Frame) {
	if f.W <= 0 || f.H <= 0 {
		return
	}
	if f.Full || f.W != l.w || f.H != l.h || len(l.rows) != f.H {
		l.w, l.h = f.W, f.H
		l.rows = make([]string, f.H)
		blank := strings.Repeat(" ", f.W)
		for i := range l.rows {
			l.rows[i] = blank
		}
	}
	for _, ln := range f.Lines {
		if ln.Y >= 0 && ln.Y < len(l.rows) {
			l.rows[ln.Y] = ln.S
		}
	}
	l.cur = f.Cursor
	l.scroll = f.Scroll
}

// closeLive tears down the current stream (if any).
func (m *model) closeLive() {
	if m.live == nil {
		return
	}
	if m.live.client != nil {
		m.live.client.Close()
	}
	if len(m.live.rows) > 0 && !m.live.exited {
		c := *m.live
		c.client = nil
		m.liveCache[c.key] = &c
	}
	m.live = nil
	m.liveFocus = false
}

// setLiveFocus routes keystrokes to claude (true) or the dashboard (false).
func (m *model) setLiveFocus(on bool) {
	m.liveFocus = on
}

// handleLiveKey forwards a key press to the focused live session, except
// for the two escape hatches that hand control back to the dashboard.
func (m *model) handleLiveKey(msg tea.KeyPressMsg, live *liveScreen) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+]", "ctrl+d":
		// Ctrl+D mirrors the fullscreen detach key; Ctrl+] is the vtspike
		// habit. Neither reaches claude.
		m.setLiveFocus(false)
		m.flash = "back to dashboard — enter or ctrl+] focuses claude again"
		return m, nil
	}
	if live.client == nil || live.exited {
		return m, nil
	}
	if err := live.client.SendKey(teaKeyToUV(msg.Key())); err != nil {
		m.err = err
	}
	return m, nil
}

// handleLivePaste forwards pasted text to the focused live session.
func (m *model) handleLivePaste(content string, live *liveScreen) {
	if live.client == nil || live.exited {
		return
	}
	if err := live.client.Paste(content); err != nil {
		m.err = err
	}
}

// forwardLiveWheel sends a wheel event to the live session when the
// pointer is over the emulator area. Returns false when it wasn't.
func (m *model) forwardLiveWheel(mm tea.Mouse) bool {
	live := m.liveForCurrent()
	if live == nil || live.client == nil || live.exited {
		return false
	}
	g, ok := m.liveScreenGeom()
	if !ok || mm.X < g.x || mm.Y < g.y || mm.X >= g.x+g.w || mm.Y >= g.y+g.h {
		return false
	}
	_ = live.client.Wheel(uv.Mouse{
		X:      mm.X - g.x,
		Y:      mm.Y - g.y,
		Button: uv.MouseButton(mm.Button),
		Mod:    uv.KeyMod(mm.Mod),
	})
	return true
}

// mouseInLiveScreen reports whether a pointer position is over the
// emulator area of the right pane.
func (m *model) mouseInLiveScreen(mm tea.Mouse) bool {
	g, ok := m.liveScreenGeom()
	if !ok {
		return false
	}
	return mm.X >= g.x && mm.Y >= g.y && mm.X < g.x+g.w && mm.Y < g.y+g.h
}

func teaKeyToUV(k tea.Key) uv.Key {
	return uv.Key{
		Text:        k.Text,
		Mod:         uv.KeyMod(k.Mod),
		Code:        k.Code,
		ShiftedCode: k.ShiftedCode,
		BaseCode:    k.BaseCode,
		IsRepeat:    k.IsRepeat,
	}
}

// liveCursor returns the real-cursor placement for View(): only while the
// live pane has focus and the child hasn't hidden its cursor. Nil hides
// the terminal cursor.
func (m *model) liveCursor() *tea.Cursor {
	if !m.liveFocus {
		return nil
	}
	live := m.liveForCurrent()
	if live == nil || live.exited || !live.cur.Visible {
		return nil
	}
	g, ok := m.liveScreenGeom()
	if !ok {
		return nil
	}
	if live.cur.X < 0 || live.cur.Y < 0 || live.cur.X >= g.w || live.cur.Y >= g.h {
		return nil
	}
	shape := tea.CursorBlock
	switch vt.CursorStyle(live.cur.Shape) {
	case vt.CursorUnderline:
		shape = tea.CursorUnderline
	case vt.CursorBar:
		shape = tea.CursorBar
	}
	return &tea.Cursor{
		Position: tea.Position{X: g.x + live.cur.X, Y: g.y + live.cur.Y},
		Shape:    shape,
		Blink:    live.cur.Blink,
	}
}

// renderLivePane draws the right pane for a live session: a status header,
// the emulator rows, and the pinned approval section. Every line is
// exactly width columns; the result has exactly height lines. We bypass
// lipgloss here because its Width() re-wraps styled text and the rows are
// already cell-exact.
func (m *model) renderLivePane(live *liveScreen, width, height int) string {
	var hdr string
	switch {
	case live.connecting:
		hdr = statusIdle.Render("⬡ live") + "  " + subtitleStyle.Render(shortID(m.currentSessionID())+" · connecting…")
	case live.scroll > 0 && !live.exited:
		hdr = statusIdle.Render(fmt.Sprintf("⬡ live · ↑ %d lines back", live.scroll)) + "  " + subtitleStyle.Render("wheel down to return · typing jumps back")
	case live.exited:
		hdr = statusStop.Render("⬡ ended") + "  " + subtitleStyle.Render(shortID(m.currentSessionID())+" · screen frozen until the server drops it")
	case m.liveFocus:
		hdr = statusActive.Render("⬡ live · typing → claude") + "  " + subtitleStyle.Render("ctrl+] back to dashboard · drag to copy · F fullscreen")
	default:
		hdr = statusIdle.Render("⬡ live") + "  " + subtitleStyle.Render(shortID(m.currentSessionID())+" · enter / ctrl+] to type · F fullscreen")
	}
	hdr = fitWidth(hdr, width)

	approvalSection, approvalH := m.approvalBlock(width, height)
	screenH := height - 1 - approvalH
	if screenH < 1 {
		screenH = 1
	}
	lines := make([]string, 0, height)
	lines = append(lines, hdr)
	sel := m.liveSel
	showSel := sel.shown && sel.key == live.key
	for y := 0; y < screenH; y++ {
		row := ""
		if y < len(live.rows) {
			row = live.rows[y]
		}
		row = fitWidth(row, width)
		if showSel {
			if from, to, ok := sel.span(y, width); ok {
				row = highlightRow(row, from, to)
			}
		}
		lines = append(lines, row)
	}
	if approvalH > 0 {
		for _, l := range strings.Split(approvalSection, "\n") {
			lines = append(lines, fitWidth(l, width))
		}
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", width))
	}
	return strings.Join(lines[:height], "\n")
}

// fitWidth pads or truncates a styled line to exactly width columns.
func fitWidth(s string, width int) string {
	w := ansi.StringWidth(s)
	switch {
	case w < width:
		return s + strings.Repeat(" ", width-w)
	case w > width:
		return ansi.Truncate(s, width, "")
	}
	return s
}

// ringBellCmd writes BEL straight to the terminal. Bubble Tea v2's cell
// renderer doesn't pass control characters through the view content, and
// a lone 0x07 can't corrupt the frame (no cursor movement), so this is
// the least intrusive way to get the terminal's attention.
func ringBellCmd() tea.Cmd {
	return func() tea.Msg {
		_, _ = os.Stdout.WriteString("\a")
		return nil
	}
}
