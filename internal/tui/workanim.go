package tui

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

// The TUI twin of the portal's working beam (runBeam in web/app.js): a
// comet of color runs round a grid tile's frame, or left to right across
// a list row, once every loop whatever the size, like the portal's
// constant-time loop. Its cells shade through beamStops, tail → head, one
// shade per cell.
var beamStops = []string{"#2dd4bf", "#38bdf8", "#60a5fa", "#a78bfa", "#e879f9", "#f472b6", "#fb923c", "#fbbf24"}

// beamLoopTicks is one lap of a list row in animTicks (28 × 75 ms ≈
// 2.1 s); a grid tile's frame is far longer, so its lap takes twice that
// (gridLoopTicks) or the light races.
const (
	beamLoopTicks = 28
	gridLoopTicks = 2 * beamLoopTicks
)

// beamRanks returns the lit cells of a perimeter of n cells at tick, one
// lap per loop ticks: perimeter index → rank (0 = the tail end, length-1
// = the head). Every lit row moves in step, and the phase follows the
// shared tick so a redraw carries on where it was.
func beamRanks(tick, n, loop int) (ranks map[int]int, length int) {
	if n <= 0 {
		return nil, 0
	}
	length = min(max(n/4, 8), 32, n)
	t := ((tick % loop) + loop) % loop
	head := t * n / loop
	ranks = make(map[int]int, length)
	for d := 0; d < length; d++ {
		ranks[((head-d)%n+n)%n] = length - 1 - d
	}
	return ranks, length
}

// beamCells is beamRanks with each cell's shade.
func beamCells(tick, n, loop int) map[int]string {
	ranks, length := beamRanks(tick, n, loop)
	out := make(map[int]string, len(ranks))
	for p, r := range ranks {
		out[p] = beamShade(r, length)
	}
	return out
}

// beamShade is the color of rank r in a comet of length cells: beamStops
// blended evenly from the tail (r = 0) to the head (r = length-1).
func beamShade(r, length int) string {
	if length <= 1 {
		return beamStops[len(beamStops)-1]
	}
	f := float64(r) / float64(length-1) * float64(len(beamStops)-1)
	i := min(int(f), len(beamStops)-2)
	return blendHex(beamStops[i], beamStops[i+1], f-float64(i))
}

// blendHex mixes two "#rrggbb" colors, t = 0 → a, 1 → b.
func blendHex(a, b string, t float64) string {
	var ar, ag, ab, br, bg, bb int
	fmt.Sscanf(a, "#%02x%02x%02x", &ar, &ag, &ab)
	fmt.Sscanf(b, "#%02x%02x%02x", &br, &bg, &bb)
	mix := func(x, y int) int { return x + int(math.Round(float64(y-x)*t)) }
	return fmt.Sprintf("#%02x%02x%02x", mix(ar, br), mix(ag, bg), mix(ab, bb))
}

// beamRow lights a list row: a band across both of its lines runs left
// to right, leaves past the right edge and comes in again from the left,
// its tail trailing behind. Lit cells take the beam color as background;
// the text on them stays readable in black. Both lines are exactly width
// cells.
func beamRow(line1, line2 string, width, tick int) (string, string) {
	if width <= 0 {
		return line1, line2
	}
	lit := rowBeam(width, tick)
	return paintCells(line1, lit), paintCells(line2, lit)
}

// rowBeam is beamRow's lit columns → color. The head runs a track of
// width + length cells (so the whole band leaves before it comes back)
// at a beamLoopTicks-per-two-widths pace, whatever the width.
func rowBeam(width, tick int) map[int]string {
	length := min(max(width/2, 8), 32, width)
	n := width + length
	loop := max(1, beamLoopTicks*n/(2*width))
	t := ((tick % loop) + loop) % loop
	head := t * n / loop
	lit := make(map[int]string, length)
	for d := 0; d < length; d++ {
		if x := head - d; x >= 0 && x < width {
			lit[x] = beamShade(length-1-d, length)
		}
	}
	return lit
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
