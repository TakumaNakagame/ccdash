package tui

// Mouse text selection inside the live pane, tmux-style: drag with the
// left button to select, release to copy. The TUI owns the mouse (cell
// motion mode), so the outer terminal's own selection never sees the
// pane; this reimplements it on top of the rendered emulator rows.

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// liveSelection is a drag selection in emulator-screen coordinates
// (0,0 = top-left cell of the emulator area, not of the terminal).
type liveSelection struct {
	key      string // ptyKey the selection belongs to
	ax, ay   int    // anchor (where the button went down)
	bx, by   int    // head (current / release position)
	dragging bool   // button is held
	shown    bool   // head has moved off the anchor → render + copy
}

// ordered returns the selection's start and end in reading order.
func (s liveSelection) ordered() (sx, sy, ex, ey int) {
	if s.ay < s.by || (s.ay == s.by && s.ax <= s.bx) {
		return s.ax, s.ay, s.bx, s.by
	}
	return s.bx, s.by, s.ax, s.ay
}

// span returns the half-open column range selected on row y, or ok=false
// when the row is outside the selection. Like a terminal, a multi-row
// selection runs to the end of every row but the last.
func (s liveSelection) span(y, width int) (from, to int, ok bool) {
	sx, sy, ex, ey := s.ordered()
	if y < sy || y > ey {
		return 0, 0, false
	}
	from, to = 0, width
	if y == sy {
		from = sx
	}
	if y == ey {
		to = ex + 1
	}
	if to > width {
		to = width
	}
	if from >= to {
		return 0, 0, false
	}
	return from, to, true
}

// selectionText extracts the selected text from rendered rows, cleaned up
// for pasting elsewhere (Slack, an editor): the trailing blanks that pad
// every emulator row are dropped, blank rows at either end are dropped,
// and the left margin Claude Code indents its output with is removed —
// the first row loses its leading blanks, the rest lose their common
// indent so nested structure (lists, code) keeps its relative shape.
func selectionText(sel liveSelection, rows []string, width int) string {
	var out []string
	_, sy, _, ey := sel.ordered()
	for y := sy; y <= ey && y < len(rows); y++ {
		from, to, ok := sel.span(y, width)
		if !ok {
			continue
		}
		out = append(out, strings.TrimRight(ansi.Strip(ansi.Cut(rows[y], from, to)), blankChars))
	}
	for len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return ""
	}
	out[0] = strings.TrimLeft(out[0], blankChars)
	indent := -1
	for _, l := range out[1:] {
		if l == "" {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, blankChars)); indent < 0 || n < indent {
			indent = n
		}
	}
	if indent > 0 {
		for i := 1; i < len(out); i++ {
			if len(out[i]) >= indent {
				out[i] = out[i][indent:]
			}
		}
	}
	return strings.Join(out, "\n")
}

// blankChars are the padding characters trimmed from copied rows. All are
// single-byte so byte offsets equal column counts when dedenting.
const blankChars = " \t"

// highlightRow renders the selected columns of row in reverse video.
func highlightRow(row string, from, to int) string {
	mid := ansi.Strip(ansi.Cut(row, from, to))
	return ansi.Cut(row, 0, from) + "\x1b[0;7m" + mid + "\x1b[0m" + ansi.TruncateLeft(row, to, "")
}

// livePoint converts a terminal mouse position to emulator coordinates,
// clamped to the emulator area. ok is false when there's no live pane.
func (m *model) livePoint(mm tea.Mouse) (x, y int, ok bool) {
	g, ok := m.liveScreenGeom()
	if !ok {
		return 0, 0, false
	}
	return clamp(mm.X-g.x, 0, g.w-1), clamp(mm.Y-g.y, 0, g.h-1), true
}

// startLiveSelection begins a drag at a click inside the emulator area.
func (m *model) startLiveSelection(live *liveScreen, mm tea.Mouse) {
	x, y, ok := m.livePoint(mm)
	if !ok {
		return
	}
	m.liveSel = liveSelection{key: live.key, ax: x, ay: y, bx: x, by: y, dragging: true}
}

// dragLiveSelection extends an in-progress drag.
func (m *model) dragLiveSelection(mm tea.Mouse) {
	if !m.liveSel.dragging {
		return
	}
	x, y, ok := m.livePoint(mm)
	if !ok {
		return
	}
	m.liveSel.bx, m.liveSel.by = x, y
	if x != m.liveSel.ax || y != m.liveSel.ay {
		m.liveSel.shown = true
	}
}

// finishLiveSelection ends the drag and copies the text. The highlight
// stays until the next click or key so the operator sees what was taken.
func (m *model) finishLiveSelection(mm tea.Mouse) tea.Cmd {
	if !m.liveSel.dragging {
		return nil
	}
	m.dragLiveSelection(mm)
	m.liveSel.dragging = false
	if !m.liveSel.shown {
		m.liveSel = liveSelection{}
		return nil
	}
	live := m.liveForCurrent()
	if live == nil || live.key != m.liveSel.key {
		m.liveSel = liveSelection{}
		return nil
	}
	text := selectionText(m.liveSel, live.rows, live.w)
	if text == "" {
		return nil
	}
	m.flash = fmt.Sprintf("copied %d chars", len([]rune(text)))
	return copyToClipboard(text)
}

// clearLiveSelection drops any highlight.
func (m *model) clearLiveSelection() { m.liveSel = liveSelection{} }

// copyToClipboard sets the clipboard via OSC 52 (works over ssh when the
// terminal allows it) and, on a local macOS session, also via pbcopy —
// iTerm2 and Terminal.app ignore OSC 52 unless explicitly enabled.
func copyToClipboard(text string) tea.Cmd {
	cmds := []tea.Cmd{tea.SetClipboard(text)}
	if runtime.GOOS == "darwin" && os.Getenv("SSH_TTY") == "" {
		cmds = append(cmds, func() tea.Msg {
			c := exec.Command("pbcopy")
			c.Stdin = strings.NewReader(text)
			_ = c.Run()
			return nil
		})
	}
	return tea.Batch(cmds...)
}
