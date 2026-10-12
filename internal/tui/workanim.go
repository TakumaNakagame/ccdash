package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

// The TUI twin of the portal's working beam (runBeam in web/app.js): a
// comet in the beam's colors runs round the outer edge of a working
// session — a grid tile's frame, or the two lines of a list row — once
// every beamLoopTicks, whatever the size, like the portal's constant-time
// loop. Colors are tail → head.
var beamColors = []string{"#38bdf8", "#a78bfa", "#f472b6", "#fbbf24"}

// beamLoopTicks is one lap of a list row in animTicks (28 × 75 ms ≈
// 2.1 s); a grid tile's frame is far longer, so its lap takes twice that
// (gridLoopTicks) or the light races.
const (
	beamLoopTicks = 28
	gridLoopTicks = 2 * beamLoopTicks
)

// beamCells returns the lit cells of a perimeter of n cells at tick, one
// lap per loop ticks:
// perimeter index → color. Every lit row moves in step, and the phase
// follows the shared tick so a redraw carries on where it was.
func beamCells(tick, n, loop int) map[int]string {
	if n <= 0 {
		return nil
	}
	length := min(max(n/6, 6), 24, n)
	t := ((tick % loop) + loop) % loop
	head := t * n / loop
	out := make(map[int]string, length)
	for d := 0; d < length; d++ {
		out[((head-d)%n+n)%n] = beamColors[(length-1-d)*len(beamColors)/length]
	}
	return out
}

// beamRow lights a list row's outer edge: its two lines (each exactly
// width cells) are the top and bottom of a 2-line box, so the comet runs
// left to right along line 1 and back along line 2. Lit cells take the
// beam color as background; the text on them stays readable in black.
func beamRow(line1, line2 string, width, tick int) (string, string) {
	if width <= 0 {
		return line1, line2
	}
	top, bottom := map[int]string{}, map[int]string{}
	for p, c := range beamCells(tick, 2*width, beamLoopTicks) {
		if p < width {
			top[p] = c
		} else {
			bottom[2*width-1-p] = c
		}
	}
	return paintCells(line1, top), paintCells(line2, bottom)
}

// paintCells gives the cells of row in lit (column → color) a background
// color, keeping row's text and its styling everywhere else. A wide
// (CJK) character is lit as a whole when either of its cells is.
func paintCells(row string, lit map[int]string) string {
	if len(lit) == 0 {
		return row
	}
	type unit struct {
		from, to int
		color    string
	}
	var units []unit
	for x, c := range lit {
		u := unit{x, x + 1, c}
		switch ansi.StringWidth(ansi.Cut(row, x, x+1)) {
		case 0: // first cell of a wide char (Cut reports it at its last cell)
			u.to = x + 2
		case 2: // last cell of one
			u.from = x - 1
		}
		units = append(units, u)
	}
	sort.Slice(units, func(i, j int) bool { return units[i].from < units[j].from })
	var b strings.Builder
	at := 0
	for _, u := range units {
		if u.from < at {
			continue // the other half of a wide char already lit
		}
		b.WriteString(ansi.Cut(row, at, u.from))
		cell := ansi.Strip(ansi.Cut(row, u.from, u.to))
		b.WriteString(lipgloss.NewStyle().Background(lipgloss.Color(u.color)).Foreground(lipgloss.Color("0")).Render(cell))
		at = u.to
	}
	b.WriteString(ansi.TruncateLeft(row, at, ""))
	return b.String()
}

// workLine is line 2's lead for an active session: the tool in flight and
// how long the turn has run — "Bash: go test ./... · 1m23s", or
// "thinking · 12s" between tools. Empty without hook data (a machine
// with no ccdash hooks only knows the session is active).
func workLine(s mdl.Session, now time.Time) string {
	if s.Status != mdl.StatusActive || (s.Tool == "" && s.TurnStartedAt.IsZero()) {
		return ""
	}
	what := "thinking"
	if s.Tool != "" {
		what = s.Tool
		if sum := strings.TrimSpace(s.ToolSummary); sum != "" && sum != s.Tool {
			what += ": " + shorten(sum, 48)
		}
	}
	if s.TurnStartedAt.IsZero() {
		return what
	}
	return what + " · " + fmtElapsed(now.Sub(s.TurnStartedAt))
}

// fmtElapsed: 45s, 1m23s, 2h05m.
func fmtElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	sec := int(d / time.Second)
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh%02dm", sec/3600, sec/60%60)
}
