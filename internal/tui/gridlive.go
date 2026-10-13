package tui

// Live grid tiles: a session that runs in ccdash shows its real screen in
// its tile, from its own /pty/{key}/screen stream (the server takes any
// number of viewers). The tile's size is the PTY's size while the grid
// shows it — like tmux panes — and the right pane / fullscreen attach push
// their own size back when they take over.
//
// These streams are separate from m.live (the right pane, which is off
// while the grid is open) and carry their own message types, so a late
// frame from a closed tile can never reach the right pane.

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"

	"github.com/takumanakagame/ccmanage/internal/auth"
	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/paths"
	"github.com/takumanakagame/ccmanage/internal/screen"
)

type gridConnectedMsg struct {
	key    string
	client *screen.Client
	err    error
}

type gridFramesMsg struct {
	key    string
	client *screen.Client
	frames []screen.Frame
}

// Smallest emulator a live tile runs; a smaller tile shows the transcript.
const (
	gridLiveMinW = 30
	gridLiveMinH = 4
)

// gridLiveKey is the ptyKey a tile streams, or "" for a transcript tile.
func (m *model) gridLiveKey(s mdl.Session) string {
	if m.remote.Enabled || !m.settings.AttachEnabled {
		return ""
	}
	return m.liveKeyFor(s)
}

// tileScreen is the emulator area of a live tile at r (body-relative):
// inside the border, below the header line.
func tileScreen(r rect) (paneGeom, bool) {
	g := paneGeom{x: r.x + 1, y: r.y + 2, w: r.w - 2, h: r.h - 3}
	return g, g.w >= gridLiveMinW && g.h >= gridLiveMinH
}

// gridWant maps each live tile's key to its emulator area (body-relative).
func (m *model) gridWant() map[string]paneGeom {
	want := map[string]paneGeom{}
	if !m.gridOpen || m.pane != paneSessions {
		return want
	}
	_, bodyH := m.bodyLayout()
	tiles := m.gridSessions()
	for i, r := range gridRects(len(tiles), m.workWidth(), bodyH) {
		key := m.gridLiveKey(m.sessions[tiles[i]])
		if key == "" {
			continue
		}
		if g, ok := tileScreen(r); ok {
			want[key] = g
		}
	}
	return want
}

// focusedGridLive is the stream of the focused tile, if it's live.
func (m *model) focusedGridLive() *liveScreen {
	if !m.gridOpen || m.selSess < 0 || m.selSess >= len(m.sessions) {
		return nil
	}
	key := m.gridLiveKey(m.sessions[m.selSess])
	if key == "" {
		return nil
	}
	return m.gridLive[key]
}

// syncGrid reconciles the tile streams with the grid: dial new live
// tiles, resize the ones whose tile changed, close the rest. Runs after
// every Update, next to syncLive.
func (m *model) syncGrid() tea.Cmd {
	want := m.gridWant()
	for key, l := range m.gridLive {
		if _, ok := want[key]; !ok {
			m.closeGridLive(key, l)
		}
	}
	if m.gridTyping {
		if l := m.focusedGridLive(); l == nil || l.client == nil || l.exited {
			m.gridTyping = false
		}
	}
	focusKey := ""
	if m.gridTyping {
		focusKey = m.focusedGridLive().key
	}
	var cmds []tea.Cmd
	for key, g := range want {
		l := m.gridLive[key]
		if l == nil {
			if !m.gridConnecting[key] {
				if m.gridConnecting == nil {
					m.gridConnecting = map[string]bool{}
				}
				m.gridConnecting[key] = true
				cmds = append(cmds, dialGridCmd(key, g.w, g.h))
			}
			continue
		}
		if l.client == nil || l.exited {
			continue
		}
		if l.sentCols != g.w || l.sentRows != g.h {
			l.sentCols, l.sentRows = g.w, g.h
			if err := l.client.Resize(g.w, g.h); err != nil {
				m.err = err
			}
		}
		if focus := key == focusKey; l.focusSent != focus {
			l.focusSent = focus
			_ = l.client.Focus(focus)
		}
	}
	return tea.Batch(cmds...)
}

func dialGridCmd(key string, cols, rows int) tea.Cmd {
	return func() tea.Msg {
		addr := fmt.Sprintf("%s:%d", paths.DefaultHost, paths.DefaultPort)
		tok, _ := auth.Load()
		c, err := screen.Dial(addr, key, tok, cols, rows)
		return gridConnectedMsg{key: key, client: c, err: err}
	}
}

func waitGridCmd(c *screen.Client) tea.Cmd {
	return func() tea.Msg {
		return gridFramesMsg{key: c.Key, client: c, frames: drainFrames(c)}
	}
}

func (m *model) handleGridConnected(msg gridConnectedMsg) tea.Cmd {
	delete(m.gridConnecting, msg.key)
	if msg.err != nil {
		delete(m.ptyAlive, msg.key)
		m.flash = "live tile unavailable: " + msg.err.Error()
		return nil
	}
	g, ok := m.gridWant()[msg.key]
	if !ok || m.gridLive[msg.key] != nil {
		msg.client.Close() // the grid moved on while we were dialing
		return nil
	}
	if m.gridLive == nil {
		m.gridLive = map[string]*liveScreen{}
	}
	l := &liveScreen{key: msg.key, client: msg.client, sentCols: g.w, sentRows: g.h}
	if c, ok := m.liveCache[msg.key]; ok {
		l.rows = append([]string(nil), c.rows...)
		l.w, l.h, l.cur = c.w, c.h, c.cur
	}
	m.gridLive[msg.key] = l
	return waitGridCmd(msg.client)
}

func (m *model) handleGridFrames(msg gridFramesMsg) tea.Cmd {
	l := m.gridLive[msg.key]
	if l == nil || l.client != msg.client {
		return nil // a stream we already closed
	}
	for _, f := range msg.frames {
		switch f.Type {
		case screen.FrameTypeClosed:
			delete(m.ptyAlive, msg.key)
			m.closeGridLive(msg.key, l)
			return nil
		case screen.FrameTypeExit:
			l.exited, l.exitErr = true, f.Err
			l.applyFrame(f)
		default:
			l.applyFrame(f)
		}
	}
	if l.exited {
		return nil
	}
	return waitGridCmd(l.client)
}

// closeGridLive ends a tile's stream, keeping its last screen in the
// shared cache so the right pane (or the tile, reopened) starts from it.
func (m *model) closeGridLive(key string, l *liveScreen) {
	if l.client != nil {
		l.client.Close()
	}
	if len(l.rows) > 0 && !l.exited {
		c := *l
		c.client = nil
		m.liveCache[key] = &c
	}
	delete(m.gridLive, key)
}

// closeAllGridLive drops every tile stream (the grid closed).
func (m *model) closeAllGridLive() {
	for key, l := range m.gridLive {
		m.closeGridLive(key, l)
	}
	m.gridTyping = false
}

// handleGridTypingKey sends a key to the focused live tile; ctrl+] /
// ctrl+d hand the keyboard back to the grid.
func (m *model) handleGridTypingKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "ctrl+]", "ctrl+d":
		m.gridTyping = false
		m.flash = "back to the grid — enter types into the tile again"
		return nil
	}
	l := m.focusedGridLive()
	if l == nil || l.client == nil || l.exited {
		m.gridTyping = false
		return nil
	}
	if err := l.client.SendKey(teaKeyToUV(msg.Key())); err != nil {
		m.err = err
	}
	return nil
}

// gridTileAt finds the tile under a screen position: its index among the
// tiles and its rect (body-relative).
func (m *model) gridTileAt(mm tea.Mouse) (int, rect, bool) {
	bodyTop, bodyH := m.bodyLayout()
	tiles := m.gridSessions()
	for i, r := range gridRects(len(tiles), m.workWidth(), bodyH) {
		if mm.X >= r.x && mm.X < r.x+r.w && mm.Y >= bodyTop+r.y && mm.Y < bodyTop+r.y+r.h {
			return i, r, true
		}
	}
	return 0, rect{}, false
}

// gridWheel forwards a wheel over a live tile to its claude (or scrolls
// that viewer back, server-side). Transcript tiles ignore it.
func (m *model) gridWheel(mm tea.Mouse) {
	i, r, ok := m.gridTileAt(mm)
	if !ok {
		return
	}
	l := m.gridLive[m.gridLiveKey(m.sessions[m.gridSessions()[i]])]
	g, ok := tileScreen(r)
	if l == nil || l.client == nil || l.exited || !ok {
		return
	}
	bodyTop, _ := m.bodyLayout()
	_ = l.client.Wheel(uv.Mouse{
		X:      clamp(mm.X-g.x, 0, g.w-1),
		Y:      clamp(mm.Y-bodyTop-g.y, 0, g.h-1),
		Button: uv.MouseButton(mm.Button),
		Mod:    uv.KeyMod(mm.Mod),
	})
}

// gridCursor puts the terminal's real cursor (the IME anchor) on the
// focused tile's emulator cursor while typing into it.
func (m *model) gridCursor() *tea.Cursor {
	if !m.gridTyping {
		return nil
	}
	l := m.focusedGridLive()
	if l == nil || l.exited || !l.cur.Visible {
		return nil
	}
	bodyTop, bodyH := m.bodyLayout()
	tiles := m.gridSessions()
	i := indexOf(tiles, m.selSess)
	rects := gridRects(len(tiles), m.workWidth(), bodyH)
	if i < 0 || i >= len(rects) {
		return nil
	}
	g, ok := tileScreen(rects[i])
	if !ok || l.cur.X < 0 || l.cur.Y < 0 || l.cur.X >= g.w || l.cur.Y >= g.h {
		return nil
	}
	shape := tea.CursorBlock
	switch vt.CursorStyle(l.cur.Shape) {
	case vt.CursorUnderline:
		shape = tea.CursorUnderline
	case vt.CursorBar:
		shape = tea.CursorBar
	}
	return &tea.Cursor{
		Position: tea.Position{X: g.x + l.cur.X, Y: bodyTop + g.y + l.cur.Y},
		Shape:    shape,
		Blink:    l.cur.Blink,
	}
}
