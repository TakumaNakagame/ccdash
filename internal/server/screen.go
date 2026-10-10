package server

// Server-side terminal emulation for PTY sessions.
//
// Every PTY the server spawns (POST /pty/start) is wrapped in an x/vt
// emulator that consumes the child's output continuously, whether or not
// anybody is looking. The TUI's right pane subscribes to a rendered view of
// that emulator via GET /pty/{key}/screen and pushes keys back through the
// same stream. Because the emulator lives here, the screen survives TUI
// restarts and is already populated the moment a viewer connects — no
// SIGWINCH / Ctrl+L nudge needed.
//
// The legacy raw relay (GET /pty/{key}/stream, used by the fullscreen
// attach) still works: while a raw client is connected the child's bytes
// are tee'd to it and the emulator's own DSR/DA replies are suppressed so
// the operator's real terminal is the one answering queries.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"github.com/takumanakagame/ccmanage/internal/attach"
	"github.com/takumanakagame/ccmanage/internal/screen"
)

// frameInterval caps how often a single viewer receives a frame. 16 ms is
// one 60 Hz tick — fast enough that typing feels immediate, slow enough
// that a chatty child (progress spinners) can't flood the TUI renderer.
const frameInterval = 16 * time.Millisecond

const (
	defaultPTYCols = 80
	defaultPTYRows = 24
)

// ptyEntry is one long-lived PTY session managed by the server. It survives
// TUI restarts; the TUI is just a viewer while connected.
type ptyEntry struct {
	sess   *attach.Session
	ptyKey string // always set — pid-<N> at creation, then also registered under sessionID

	// connected is true while a raw (fullscreen) stream is active. Only one
	// raw client at a time; screen viewers are unlimited.
	connected atomic.Bool

	// emuMu guards emu and the cursor mirror below. Every emulator call
	// except Read goes through it; Read is served by the response pump
	// which never takes the lock.
	emuMu sync.Mutex
	emu   *vt.Emulator
	tag   int // hook tag handed to the child; see ptyTagBase
	// project / projectDone: the project the spawn joins (applyPTYProjects);
	// guarded by Server.ptyMu.
	project     string
	projectDone bool
	sf          strFilter      // scrubs C1-looking bytes out of OSC etc.; see strfilter.go
	trace       io.WriteCloser // raw child output dump; nil unless CCDASH_PTY_TRACE_DIR is set
	curHidden   bool
	curStyle    vt.CursorStyle
	curBlink    bool
	// mouseModes mirrors the child's mouse-tracking modes (guarded by
	// emuMu). With none set the emulator drops wheel events, so the
	// screen stream scrolls its own scrollback instead; see wheelScrolls.
	mouseModes map[ansi.Mode]bool

	// rawMu guards raw, the optional fullscreen relay sink.
	rawMu sync.Mutex
	raw   io.Writer

	viewersMu sync.Mutex
	viewers   map[*screenViewer]struct{}

	exited  atomic.Bool
	exitErr atomic.Pointer[string]
}

// screenViewer is one connected /screen client.
type screenViewer struct {
	entry *ptyEntry
	conn  net.Conn
	enc   *json.Encoder

	kick chan struct{} // cap 1; "something changed, send a frame"
	stop chan struct{} // closed by the handler when the client goes away
	done chan struct{} // closed by serve when it exits

	// last holds the rows sent in the previous frame so we can diff.
	// Only serve() touches these.
	last      []string
	lastW     int
	lastH     int
	lastFlush time.Time

	// needFull is set by resize (any goroutine) and consumed by serve.
	needFull atomic.Bool

	// scroll is how many lines this viewer is scrolled back into the
	// main screen's scrollback (0 = live). Wheel input moves it when the
	// child doesn't track the mouse; typing returns to 0. lastSB is the
	// scrollback length at the previous frame so a scrolled view stays
	// put while new output arrives. Both guarded by entry.emuMu.
	scroll int
	lastSB int
}

// newPTYEntry wires an attach.Session (already constructed, not yet
// started) to a fresh emulator of the given size. Call sess.Start after.
func newPTYEntry(sess *attach.Session, key string, cols, rows int) *ptyEntry {
	if cols <= 0 {
		cols = defaultPTYCols
	}
	if rows <= 0 {
		rows = defaultPTYRows
	}
	e := &ptyEntry{
		sess:       sess,
		ptyKey:     key,
		emu:        vt.NewEmulator(cols, rows),
		curBlink:   true,
		viewers:    map[*screenViewer]struct{}{},
		mouseModes: map[ansi.Mode]bool{},
	}
	e.emu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { e.curHidden = !visible },
		CursorStyle: func(style vt.CursorStyle, blink bool) {
			e.curStyle = style
			e.curBlink = blink
		},
		EnableMode: func(m ansi.Mode) {
			if isMouseMode(m) {
				e.mouseModes[m] = true
			}
		},
		DisableMode: func(m ansi.Mode) { delete(e.mouseModes, m) },
	})
	e.trace = openPTYTrace(key)
	sess.InitialSize = &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}
	// Child output → emulator (+ raw relay when attached).
	sess.SetSink(e)
	return e
}

// startPumps launches the goroutines that need a running child: the
// emulator → PTY response pump and the exit watcher. Call once after
// sess.Start succeeded.
func (e *ptyEntry) startPumps() {
	go e.responsePump()
	go func() {
		<-e.sess.ChildExit()
		e.onExit()
	}()
}

// Write implements io.Writer for attach.Session's sink: child bytes go
// into the emulator and, when a fullscreen client is attached, to it too.
func (e *ptyEntry) Write(p []byte) (int, error) {
	e.emuMu.Lock()
	if e.trace != nil {
		_, _ = e.trace.Write(p)
	}
	_, _ = e.emu.Write(e.sf.filter(p))
	e.emuMu.Unlock()

	e.rawMu.Lock()
	raw := e.raw
	e.rawMu.Unlock()
	if raw != nil {
		_, _ = raw.Write(p)
	}
	e.kickViewers()
	return len(p), nil
}

// responsePump drains the emulator's reply stream (key encodings, DSR/DA
// answers, bracketed paste markers) into the PTY. While a raw fullscreen
// client is connected the operator's real terminal answers queries, so
// the emulator's copies are dropped to avoid double replies.
func (e *ptyEntry) responsePump() {
	buf := make([]byte, 4096)
	for {
		n, err := e.emu.Read(buf)
		if n > 0 && !e.connected.Load() {
			if f := e.sess.Pty(); f != nil {
				_, _ = f.Write(buf[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

func (e *ptyEntry) onExit() {
	if e.exited.Swap(true) {
		return
	}
	if err := e.sess.ExitErr(); err != nil {
		msg := err.Error()
		e.exitErr.Store(&msg)
	}
	// Unblock responsePump by closing the emulator's reply pipe directly.
	// Emulator.Close would also flip an unsynchronized "closed" flag that
	// Read polls without a lock (a data race under -race); closing just
	// the pipe delivers the same EOF without touching shared state.
	if pw, ok := e.emu.InputPipe().(*io.PipeWriter); ok {
		_ = pw.CloseWithError(io.EOF)
	} else {
		e.emuMu.Lock()
		_ = e.emu.Close()
		e.emuMu.Unlock()
	}
	e.emuMu.Lock()
	if e.trace != nil {
		_ = e.trace.Close()
		e.trace = nil
	}
	e.emuMu.Unlock()
	e.kickViewers()
}

// openPTYTrace opens a file receiving the child's raw output when the
// collector runs with CCDASH_PTY_TRACE_DIR set — a debugging aid for
// emulator rendering bugs (replay the bytes into x/vt vs a real terminal).
// Returns nil when tracing is off or the file can't be created.
func openPTYTrace(key string) io.WriteCloser {
	dir := os.Getenv("CCDASH_PTY_TRACE_DIR")
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("pty trace: %v", err)
		return nil
	}
	name := fmt.Sprintf("%s-%d.raw", strings.ReplaceAll(key, "/", "_"), time.Now().Unix())
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("pty trace: %v", err)
		return nil
	}
	return f
}

// setRaw installs (or clears, with nil) the fullscreen relay sink.
func (e *ptyEntry) setRaw(w io.Writer) {
	e.rawMu.Lock()
	e.raw = w
	e.rawMu.Unlock()
}

// resize applies a new geometry to both the emulator and the PTY. Every
// viewer gets a full frame next flush because row contents shift.
func (e *ptyEntry) resize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	e.emuMu.Lock()
	if !e.exited.Load() {
		e.emu.Resize(cols, rows)
	}
	e.emuMu.Unlock()
	if f := e.sess.Pty(); f != nil {
		_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
	e.viewersMu.Lock()
	for v := range e.viewers {
		v.needFull.Store(true)
	}
	e.viewersMu.Unlock()
	e.kickViewers()
}

func (e *ptyEntry) size() (cols, rows int) {
	e.emuMu.Lock()
	defer e.emuMu.Unlock()
	return e.emu.Width(), e.emu.Height()
}

func (e *ptyEntry) kickViewers() {
	e.viewersMu.Lock()
	for v := range e.viewers {
		select {
		case v.kick <- struct{}{}:
		default:
		}
	}
	e.viewersMu.Unlock()
}

func (e *ptyEntry) addViewer(v *screenViewer) {
	e.viewersMu.Lock()
	e.viewers[v] = struct{}{}
	e.viewersMu.Unlock()
}

func (e *ptyEntry) removeViewer(v *screenViewer) {
	e.viewersMu.Lock()
	delete(e.viewers, v)
	e.viewersMu.Unlock()
}

// isMouseMode reports whether m is one of the modes under which the
// emulator forwards mouse events to the child (vt.Emulator.SendMouse).
func isMouseMode(m ansi.Mode) bool {
	switch m {
	case ansi.ModeMouseX10, ansi.ModeMouseNormal, ansi.ModeMouseHighlight,
		ansi.ModeMouseButtonEvent, ansi.ModeMouseAnyEvent:
		return true
	}
	return false
}

// wheelScrollStep is how many lines one wheel notch scrolls back.
const wheelScrollStep = 3

// wheel handles a wheel event from v. A child that tracks the mouse
// (Claude Code's fullscreen renderer) gets it; otherwise — the classic
// renderer, a shell — the emulator would drop it, so it scrolls v's view
// through the main screen's scrollback the way a real terminal does.
func (e *ptyEntry) wheel(v *screenViewer, m uv.Mouse) {
	e.emuMu.Lock()
	defer e.emuMu.Unlock()
	if e.exited.Load() {
		return
	}
	if len(e.mouseModes) > 0 {
		e.emu.SendMouse(uv.MouseWheelEvent(m))
		return
	}
	if e.emu.IsAltScreen() {
		return
	}
	switch m.Button {
	case uv.MouseWheelUp:
		v.scroll = min(v.scroll+wheelScrollStep, e.emu.ScrollbackLen())
	case uv.MouseWheelDown:
		v.scroll = max(v.scroll-wheelScrollStep, 0)
	default:
		return
	}
	v.lastSB = e.emu.ScrollbackLen()
	select {
	case v.kick <- struct{}{}:
	default:
	}
}

// snapshot renders the live emulator screen into per-row strings (each
// exactly width columns, style reset at the end) plus the cursor state.
func (e *ptyEntry) snapshot() (rows []string, w, h int, cur screen.Cursor) {
	rows, w, h, _, cur = e.snapshotFor(nil)
	return rows, w, h, cur
}

// snapshotFor is snapshot as seen by viewer v: scrolled back into the
// scrollback when v.scroll > 0. A nil v gets the live screen.
func (e *ptyEntry) snapshotFor(v *screenViewer) (rows []string, w, h, scroll int, cur screen.Cursor) {
	e.emuMu.Lock()
	raw := e.emu.Render()
	pos := e.emu.CursorPosition()
	w, h = e.emu.Width(), e.emu.Height()
	cur = screen.Cursor{
		X:       pos.X,
		Y:       pos.Y,
		Visible: !e.curHidden,
		Shape:   int(e.curStyle),
		Blink:   e.curBlink,
	}
	var back []string
	if v != nil && v.scroll > 0 {
		if e.emu.IsAltScreen() {
			v.scroll = 0
		} else {
			sb := e.emu.Scrollback()
			n := sb.Len()
			// Keep the same lines in view while output keeps coming.
			v.scroll = min(v.scroll+max(n-v.lastSB, 0), n)
			v.lastSB = n
			for i := n - v.scroll; i < n && len(back) < h; i++ {
				back = append(back, sb.Line(i).Render())
			}
		}
	}
	if v != nil {
		scroll = v.scroll
	}
	e.emuMu.Unlock()
	rows = padRows(raw, w, h)
	if scroll > 0 {
		rows = padRows(strings.Join(append(back, rows...)[:h], "\n"), w, h)
		cur.Visible = false
	}
	return rows, w, h, scroll, cur
}

// padRows splits a rendered screen into exactly h rows of exactly w
// display columns, terminating each with a style reset so a row can be
// spliced into any surrounding content without leaking attributes.
func padRows(raw string, w, h int) []string {
	rows := strings.Split(raw, "\n")
	for len(rows) < h {
		rows = append(rows, "")
	}
	rows = rows[:h]
	for i, r := range rows {
		if pad := w - ansi.StringWidth(r); pad > 0 {
			r += strings.Repeat(" ", pad)
		} else if pad < 0 {
			r = ansi.Truncate(r, w, "")
		}
		rows[i] = r + ansi.ResetStyle
	}
	return rows
}

// diffRows returns the rows in cur that differ from prev (or all rows when
// full / geometry changed).
func diffRows(prev, cur []string, full bool) []screen.Line {
	if full || len(prev) != len(cur) {
		out := make([]screen.Line, len(cur))
		for i, s := range cur {
			out[i] = screen.Line{Y: i, S: s}
		}
		return out
	}
	var out []screen.Line
	for i, s := range cur {
		if s != prev[i] {
			out = append(out, screen.Line{Y: i, S: s})
		}
	}
	return out
}

// buildFrame produces the next frame for v. Must be called from v's own
// goroutine (it mutates v.last).
func (v *screenViewer) buildFrame() screen.Frame {
	rows, w, h, scroll, cur := v.entry.snapshotFor(v)
	full := v.needFull.Swap(false) || v.lastW != w || v.lastH != h
	lines := diffRows(v.last, rows, full)
	v.last, v.lastW, v.lastH = rows, w, h
	f := screen.Frame{Type: screen.FrameTypeFrame, W: w, H: h, Full: full, Lines: lines, Cursor: cur, Scroll: scroll}
	if v.entry.exited.Load() {
		f.Type = screen.FrameTypeExit
		if p := v.entry.exitErr.Load(); p != nil {
			f.Err = *p
		}
	}
	return f
}

// serve pushes frames to the viewer until the connection or the child
// goes away. Frames are coalesced to frameInterval.
func (v *screenViewer) serve() {
	defer close(v.done)
	// First frame immediately (full).
	v.needFull.Store(true)
	if err := v.enc.Encode(v.buildFrame()); err != nil {
		return
	}
	v.lastFlush = time.Now()
	for {
		select {
		case <-v.kick:
		case <-v.stop:
			return
		}
		if d := time.Since(v.lastFlush); d < frameInterval {
			time.Sleep(frameInterval - d)
		}
		// Drop a kick that landed during the sleep — the frame we're about
		// to build already reflects it.
		select {
		case <-v.kick:
		default:
		}
		// Cursor-only updates still matter (the IME anchors on the cursor),
		// so a frame goes out even when no row changed.
		f := v.buildFrame()
		v.lastFlush = time.Now()
		if err := v.enc.Encode(f); err != nil {
			return
		}
		if f.Type == screen.FrameTypeExit {
			return
		}
	}
}

// handlePTYScreen upgrades the connection to a screen stream: JSON frames
// out, JSON inputs in. Any number of viewers may share one entry.
func (s *Server) handlePTYScreen(w http.ResponseWriter, r *http.Request, id string) {
	entry := s.lookupPTY(id)
	if entry == nil {
		http.Error(w, "pty not found", http.StatusNotFound)
		return
	}
	if !entry.sess.Alive() {
		http.Error(w, "pty session exited", http.StatusGone)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("hijack: %v", err), http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: %s\r\nConnection: Upgrade\r\n\r\n", screen.UpgradeProtocol)
	_ = brw.Flush()

	v := &screenViewer{
		entry: entry,
		conn:  conn,
		enc:   json.NewEncoder(conn),
		kick:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	entry.addViewer(v)
	defer entry.removeViewer(v)
	go v.serve()

	// Input loop runs on the handler goroutine.
	dec := json.NewDecoder(bufio.NewReader(brw))
	for {
		var in screen.Input
		if err := dec.Decode(&in); err != nil {
			break
		}
		switch in.Type {
		case screen.InputTypeWheel:
			if in.Mouse != nil {
				entry.wheel(v, *in.Mouse)
			}
			continue
		case screen.InputTypeKey, screen.InputTypePaste:
			// Typing goes to the live screen, like a terminal snapping
			// back to the bottom.
			entry.emuMu.Lock()
			v.scroll = 0
			entry.emuMu.Unlock()
		}
		entry.applyInput(in)
	}
	// Tell serve() to stop and wait for it so we don't close conn under an
	// in-flight Encode. Bounded: a stuck Encode is cut by the deferred
	// conn.Close anyway.
	entry.removeViewer(v)
	close(v.stop)
	select {
	case <-v.done:
	case <-time.After(500 * time.Millisecond):
	}
}

// applyInput dispatches one client message into the emulator / PTY.
func (e *ptyEntry) applyInput(in screen.Input) {
	switch in.Type {
	case screen.InputTypeKey:
		if in.Key == nil {
			return
		}
		e.emuMu.Lock()
		if !e.exited.Load() {
			for _, k := range normalizeKey(*in.Key) {
				e.emu.SendKey(k)
			}
		}
		e.emuMu.Unlock()
	case screen.InputTypePaste:
		e.emuMu.Lock()
		if !e.exited.Load() {
			e.emu.Paste(in.Text)
		}
		e.emuMu.Unlock()
	case screen.InputTypeResize:
		e.resize(in.Cols, in.Rows)
	case screen.InputTypeFocus:
		e.emuMu.Lock()
		if !e.exited.Load() {
			if in.Focus {
				e.emu.Focus()
			} else {
				e.emu.Blur()
			}
		}
		e.emuMu.Unlock()
	}
}

// textMods are modifiers that don't change what a printable key means:
// the character itself is already in Key.Text.
const textMods = uv.ModShift | uv.ModCapsLock | uv.ModNumLock | uv.ModScrollLock

// normalizeKey turns a key that produced text into plain unmodified key
// presses, one per rune. x/vt's encoder only emits printable keys whose
// Mod is 0, so Shift+A (Code 'a', Text "A", Mod Shift) would otherwise
// be dropped. Keys carrying Ctrl/Alt/etc. — or no text — pass through
// unchanged so the emulator's control-sequence table still handles them.
func normalizeKey(k uv.Key) []uv.KeyPressEvent {
	if k.Text == "" || k.Mod&^textMods != 0 {
		return []uv.KeyPressEvent{uv.KeyPressEvent(k)}
	}
	out := make([]uv.KeyPressEvent, 0, len(k.Text))
	for _, r := range k.Text {
		out = append(out, uv.KeyPressEvent{Code: r, Text: string(r)})
	}
	return out
}

// ptyInfo is one row of GET /pty.
type ptyInfo struct {
	Key   string `json:"key"`
	Alive bool   `json:"alive"`
	PID   int    `json:"pid"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
}

// handlePTYList reports every registered ptyKey (aliases included) so the
// TUI can decide which rows have a live screen without probing each one.
func (s *Server) handlePTYList(w http.ResponseWriter, _ *http.Request) {
	s.ptyMu.Lock()
	out := make([]ptyInfo, 0, len(s.ptyMap))
	for k, e := range s.ptyMap {
		cols, rows := e.size()
		out = append(out, ptyInfo{Key: k, Alive: e.sess.Alive(), PID: e.sess.PID(), Cols: cols, Rows: rows})
	}
	s.ptyMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("pty list: %v", err)
	}
}
