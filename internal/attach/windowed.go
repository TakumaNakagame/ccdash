// windowed.go layers a vt10x terminal emulator on top of attach.Session so
// the TUI can render claude's screen state inside its right pane instead of
// taking over the operator's whole terminal.
//
// The PTY pump in attach.go already drains bytes — we hook in by tee-ing
// every chunk into a vt10x.Terminal kept in this file. RenderWindowed
// reads that emulator's cell grid and emits a plain ANSI string that
// Bubble Tea / lipgloss can splice into a layout. We only emit SGR
// (color / style) escapes plus text + newlines — no cursor positioning —
// so the framework's row-by-row layout stays in charge.
//
// Cell-attribute bit positions are taken straight from vt10x's private
// `attrReverse … attrWrap` constants. The package doesn't export them but
// they've been stable since 2022; the alternative would be forking vt10x.
package attach

import (
	"fmt"
	"strings"
	"sync"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
	"github.com/mattn/go-runewidth"
)

// Mirror of vt10x's private attribute bits. Keep in sync with state.go in
// upstream vt10x; the file is unlikely to change but breakage here would
// only affect text styling (not correctness of the cell content).
const (
	vtAttrReverse   int16 = 1 << 0
	vtAttrUnderline int16 = 1 << 1
	vtAttrBold      int16 = 1 << 2
	_               int16 = 1 << 3 // attrGfx — graphics char set, ignored in render
	vtAttrItalic    int16 = 1 << 4
	vtAttrBlink     int16 = 1 << 5
	_               int16 = 1 << 6 // attrWrap — line-wrap marker, irrelevant here
)

// initialWindowedCols / Rows are the size we hand vt10x before the TUI has
// told us its real layout. Bubble Tea sends a window size message on
// startup so this only matters for the first render or two.
const (
	initialWindowedCols = 80
	initialWindowedRows = 24
)

// Notify size for the output channel — on each PTY chunk we drop a
// non-blocking ping so the TUI can re-render. Channel capacity 1 is enough
// because we coalesce; missing a notify just delays the next paint by one
// tick of Bubble Tea's normal event loop, which is fine.
const outputNotifyCap = 1

// initWindowed wires the emulator + output channel into a Session that has
// already had spawn() run. Safe to call multiple times; subsequent calls
// are no-ops because of windowedOnce.
func (s *Session) initWindowed() {
	s.windowedOnce.Do(func() {
		s.vt = vt10x.New(vt10x.WithSize(initialWindowedCols, initialWindowedRows))
		s.outputCh = make(chan struct{}, outputNotifyCap)
	})
}

// notifyOutput drops a non-blocking ping into outputCh so the TUI's
// subscriber can re-render. Coalesced: if the channel already has a
// pending notify the new one is dropped (the subscriber will pick up the
// latest state on its next read either way).
func (s *Session) notifyOutput() {
	if s.outputCh == nil {
		return
	}
	select {
	case s.outputCh <- struct{}{}:
	default:
	}
}

// OutputCh returns a channel that fires whenever the PTY produced new
// output. The channel is created lazily on the first windowed render;
// before that it's nil. Callers that need notifications should call
// EnsureWindowed first.
func (s *Session) OutputCh() <-chan struct{} {
	return s.outputCh
}

// EnsureWindowed makes sure the windowed render path is initialised. Call
// it once after Start (or the first Attach) before relying on the output
// channel or RenderWindowed.
func (s *Session) EnsureWindowed() {
	s.initWindowed()
}

// SendInput writes raw bytes to the PTY without going through the
// fullscreen attach path. Used by the TUI when the operator focuses the
// right-pane attach view and types — keystrokes are forwarded the same
// way the in-process stdin pump does, but without raw-mode setup or Ctrl+D
// interception (the TUI is the one in charge of the keyboard now, so it
// also owns the escape semantics).
func (s *Session) SendInput(b []byte) error {
	if s == nil || s.pty == nil {
		return errOpAfterClose
	}
	_, err := s.pty.Write(b)
	return err
}

// Resize pushes new dimensions to both the emulator (so future cell
// queries return the right grid) and the PTY (so claude gets a SIGWINCH
// and redraws).
func (s *Session) Resize(cols, rows int) error {
	if cols < 1 || rows < 1 {
		return nil
	}
	if s.vt != nil {
		s.vt.Resize(cols, rows)
	}
	if s.pty != nil {
		return pty.Setsize(s.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
	return nil
}

// RenderWindowed walks the emulator's cell grid and produces a string of
// "<ANSI styles><char><ANSI styles><char>…\n…" suitable for embedding in a
// Bubble Tea layout. Width / height clip to the requested rectangle even
// if the emulator was resized to a different size in the meantime — that
// way a stale render between Resize calls just shows a partial view
// instead of blowing up.
func (s *Session) RenderWindowed(width, height int) string {
	if s == nil || s.vt == nil || width < 1 || height < 1 {
		return ""
	}
	s.vt.Lock()
	defer s.vt.Unlock()

	emCols, emRows := s.vt.Size()
	if width > emCols {
		width = emCols
	}
	if height > emRows {
		height = emRows
	}
	cur := s.vt.Cursor()
	curVisible := s.vt.CursorVisible()

	var b strings.Builder
	// Pre-grow: every cell costs ~8-12 ANSI bytes worst case, plus newlines.
	b.Grow(width*height*12 + height)

	// Track previous cell attributes so we only emit a new SGR when
	// something actually changed — keeps the ANSI volume reasonable on
	// flat regions of the screen.
	//
	// Visual-width handling: vt10x stores one rune per grid cell and
	// advances its cursor by one regardless of the character's display
	// width. That breaks down for CJK / emoji / box-drawing wide chars,
	// which the terminal will render as 2 cells. We compensate by
	// counting visual width as we walk and stopping a row at the pane
	// width — emitting fewer cells from the grid is preferable to
	// letting the terminal auto-wrap our line, which is what produces
	// the "characters scattered everywhere" symptom.
	var prev vt10x.Glyph
	first := true
	for y := 0; y < height; y++ {
		used := 0
		for x := 0; x < width && used < width; x++ {
			cell := s.vt.Cell(x, y)
			isCursor := curVisible && y == cur.Y && x == cur.X
			ch := cell.Char
			if ch == 0 {
				ch = ' '
			}
			rw := runewidth.RuneWidth(ch)
			if rw <= 0 {
				rw = 1
			}
			if used+rw > width {
				// A wide char that wouldn't fit gets replaced with a
				// space so the row visual width still hits exactly
				// `width`. Without this we'd either overflow (auto-wrap
				// → scattered text) or undercount.
				ch = ' '
				rw = 1
			}
			if first || !sameAttrs(prev, cell, isCursor) {
				writeSGR(&b, cell, isCursor)
				prev = cell
				first = false
			}
			b.WriteRune(ch)
			used += rw
		}
		// Pad the rest of the row with spaces so the line's visual
		// width is exactly the pane width. Mismatched widths are what
		// triggers the terminal-side auto-wrap that scrambles the
		// rendered output.
		if used < width {
			b.WriteString("\x1b[0m")
			b.WriteString(strings.Repeat(" ", width-used))
			first = true
		} else {
			b.WriteString("\x1b[0m")
			first = true
		}
		if y < height-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// sameAttrs returns true when two glyphs would render with the same SGR
// state (so we can skip re-emitting a fresh sequence). Cursor takes
// precedence — a cursor cell always reverses regardless of underlying
// attrs, so two adjacent cells where only one is the cursor are
// considered different.
func sameAttrs(a, b vt10x.Glyph, bIsCursor bool) bool {
	if bIsCursor {
		return false
	}
	return a.Mode == b.Mode && a.FG == b.FG && a.BG == b.BG
}

// writeSGR writes the ANSI SGR sequence for the given glyph + cursor
// hint. Style bits (bold/italic/etc.) and 256-color FG/BG are translated;
// rare features (blink, dim) are mapped where ANSI has equivalents.
func writeSGR(b *strings.Builder, g vt10x.Glyph, cursor bool) {
	// Always reset first — vt10x glyphs are absolute, so we shouldn't
	// inherit the previous cell's bold-but-no-italic into a non-bold cell.
	b.WriteString("\x1b[0")
	if cursor {
		b.WriteString(";7") // reverse video for cursor
	} else {
		if g.Mode&vtAttrReverse != 0 {
			b.WriteString(";7")
		}
	}
	if g.Mode&vtAttrBold != 0 {
		b.WriteString(";1")
	}
	if g.Mode&vtAttrUnderline != 0 {
		b.WriteString(";4")
	}
	if g.Mode&vtAttrItalic != 0 {
		b.WriteString(";3")
	}
	if g.Mode&vtAttrBlink != 0 {
		b.WriteString(";5")
	}
	writeColor(b, g.FG, true)
	writeColor(b, g.BG, false)
	b.WriteByte('m')
}

// writeColor maps a vt10x.Color into an ANSI fragment. Defaults map to
// 39/49 (terminal-default fg/bg). 0–15 use the basic 30/40-series codes;
// 16–255 use the 256-color extended sequence. RGB (24-bit) glyphs aren't
// emitted by vt10x in the version we depend on, so we don't handle them.
func writeColor(b *strings.Builder, c vt10x.Color, isFG bool) {
	switch c {
	case vt10x.DefaultFG:
		if isFG {
			b.WriteString(";39")
		}
		return
	case vt10x.DefaultBG:
		if !isFG {
			b.WriteString(";49")
		}
		return
	}
	idx := uint32(c)
	if idx >= 256 {
		// Out of range for ANSI 256 — fall back to default rather than
		// emit a malformed sequence. (This shouldn't happen with vt10x
		// at HEAD, but we'd rather render plainly than garble the row.)
		if isFG {
			b.WriteString(";39")
		} else {
			b.WriteString(";49")
		}
		return
	}
	if idx < 8 {
		base := 30
		if !isFG {
			base = 40
		}
		fmt.Fprintf(b, ";%d", base+int(idx))
		return
	}
	if idx < 16 {
		// "Bright" 8 — mapped to 90/100-series in modern terminals.
		base := 90
		if !isFG {
			base = 100
		}
		fmt.Fprintf(b, ";%d", base+int(idx-8))
		return
	}
	// 256-color extended.
	prefix := 38
	if !isFG {
		prefix = 48
	}
	fmt.Fprintf(b, ";%d;5;%d", prefix, idx)
}

// errOpAfterClose is returned by SendInput when the session was already
// closed. We don't surface a richer error — the TUI just retries on the
// next keystroke, and by then the row will have been pruned out of the
// per-session map.
var errOpAfterClose = errStringErr("attach: session is closed or not started")

type errStringErr string

func (e errStringErr) Error() string { return string(e) }

// (sync.Once + emulator handle live on Session itself; the below ensures
// the import is exercised even when the helper file is the only one
// referencing vt10x.)
var _ vt10x.Terminal = (vt10x.Terminal)(nil)

// (sync import above kept for the windowedOnce field on Session.)
var _ = sync.Once{}
