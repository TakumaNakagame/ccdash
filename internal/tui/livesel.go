package tui

// Mouse text selection inside the live pane, tmux-style: drag with the
// left button to select, release to copy. The TUI owns the mouse (cell
// motion mode), so the outer terminal's own selection never sees the
// pane; this reimplements it on top of the rendered emulator rows.

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
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
// for pasting elsewhere (a shell, Slack, an editor):
//   - the trailing blanks that pad every emulator row are dropped, and
//     blank rows at either end;
//   - the gutter marks Claude Code draws in front of text — "● " message
//     bullets, "⎿ " tool output, "▎"/"│" quote bars, "❯ " prompts — are
//     removed, and a "│ … │" box's right border with them; rows that are
//     only box edges or rules go;
//   - a row Claude Code wrapped to fit the pane is joined with the next one
//     (a long command copies as one line);
//   - the left margin is removed — rows lose their common indent so nested
//     structure (lists, code) keeps its relative shape (a first row picked
//     up mid-row just loses its leading blanks).
func selectionText(sel liveSelection, rows []string, width int) string {
	type piece struct {
		text    string // selected text, gutter removed
		wrapsOn bool   // the row was filled up to the pane edge
		fullW   int    // display width of the whole row
		boxEdge bool
	}
	var ps []piece
	_, sy, _, ey := sel.ordered()
	for y := sy; y <= ey && y < len(rows); y++ {
		from, to, ok := sel.span(y, width)
		if !ok {
			continue
		}
		full := strings.TrimRight(clean(ansi.Strip(rows[y])), blankChars)
		seg := strings.TrimRight(clean(ansi.Strip(ansi.Cut(rows[y], from, to))), blankChars)
		seg = stripGutter(seg)
		fw := ansi.StringWidth(full)
		ps = append(ps, piece{text: seg, fullW: fw, wrapsOn: fw >= width-2, boxEdge: boxEdgeRow.MatchString(full)})
	}
	// Join soft-wrapped rows into logical lines.
	var out []string
	joinNext := false
	for i, p := range ps {
		if p.boxEdge {
			joinNext = false
			continue
		}
		if joinNext && len(out) > 0 {
			out[len(out)-1] += strings.TrimLeft(p.text, blankChars)
		} else {
			out = append(out, p.text)
		}
		joinNext = false
		if i+1 < len(ps) && p.text != "" && !ps[i+1].boxEdge {
			next := strings.TrimLeft(ps[i+1].text, blankChars)
			switch {
			case next == "" || gutterStart.MatchString(next):
			case p.wrapsOn:
				// Filled to the edge: a character wrap, nothing was dropped.
				joinNext = true
			case width-p.fullW <= max(12, width/4) && p.fullW+1+ansi.StringWidth(firstWord(next)) > width:
				// Ends fairly close to the edge and the next word wouldn't
				// have fit: a word wrap, which ate one space. (A short
				// "Run:" followed by a long command line is left alone.)
				out[len(out)-1] += " "
				joinNext = true
			}
		}
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
	// A selection that starts mid-row has no margin on its first row; one
	// that starts at the row's beginning dedents it with the rest.
	first := 1
	if sx, _, _, _ := sel.ordered(); sx > 0 {
		out[0] = strings.TrimLeft(out[0], blankChars)
	} else {
		first = 0
	}
	indent := -1
	for _, l := range out[first:] {
		if l == "" {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, blankChars)); indent < 0 || n < indent {
			indent = n
		}
	}
	if indent > 0 {
		for i := first; i < len(out); i++ {
			if len(out[i]) >= indent {
				out[i] = out[i][indent:]
			}
		}
	}
	for i := range out {
		out[i] = strings.TrimRight(out[i], blankChars)
	}
	return strings.Join(out, "\n")
}

var (
	// gutterMark: a mark Claude Code draws in front of text, then its
	// spacing. Only these — an ASCII "|" or ">" may be real content.
	gutterMark  = regexp.MustCompile(`^( *)([●⏺⎿▎▍▌│┃❯])( +|$)`)
	gutterStart = regexp.MustCompile(`^[●⏺⎿❯]( |$)`)
	boxEdgeRow  = regexp.MustCompile(`^[ ]*(?:[╭╰┌└├][─━┄┈ ]*[╮╯┐┘┤]?|[─━═┄┈]{3,})[ ]*$`)
)

// clean turns the no-break spaces Claude Code pads marks with into plain
// spaces (so trimming and column math treat them as blanks).
func clean(s string) string { return strings.ReplaceAll(s, "\u00a0", " ") }

// stripGutter blanks out a leading gutter mark (keeping the columns, so the
// dedent below still lines rows up) and, for a "│ … │" box row, the right
// border.
func stripGutter(s string) string {
	m := gutterMark.FindStringSubmatchIndex(s)
	if m == nil {
		return s
	}
	mark := s[m[4]:m[5]]
	rest := s[m[1]:]
	s = s[:m[3]] + strings.Repeat(" ", 1+(m[7]-m[6])) + rest
	if mark == "│" || mark == "┃" {
		if t := strings.TrimRight(s, blankChars); strings.HasSuffix(t, mark) {
			s = strings.TrimRight(strings.TrimSuffix(t, mark), blankChars)
		}
	}
	return s
}

// firstWord is s up to its first blank.
func firstWord(s string) string {
	if i := strings.IndexAny(s, blankChars); i >= 0 {
		return s[:i]
	}
	return s
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
