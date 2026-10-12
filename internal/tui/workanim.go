package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

// The TUI twin of the portal's working beam (runBeam in web/app.js): a
// short comet in the beam's colors runs left to right across the title of
// a working session, one cell per animTick. Colors are tail → head.
var sweepColors = []string{"#38bdf8", "#a78bfa", "#f472b6", "#fbbf24"}

// sweepGap is how many ticks the comet spends off the end of the title
// before it comes round again, so a short title isn't one constant blur.
const sweepGap = 14

// sweep renders text in base with the comet's cells recolored. Columns
// are cells, not runes, so a wide (CJK) character is lit when the head
// reaches either of its cells. tick follows the shared animTick, so every
// working row moves in step and a redraw carries on where it was.
func sweep(text string, tick int, base lipgloss.Style) string {
	w := runewidth.StringWidth(text)
	if w == 0 {
		return ""
	}
	period := w + len(sweepColors) + sweepGap
	head := ((tick % period) + period) % period
	var b, run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			b.WriteString(base.Render(run.String()))
			run.Reset()
		}
	}
	col := 0
	for _, r := range text {
		cw := runewidth.RuneWidth(r)
		// Distance behind the head of this rune's nearest cell.
		d := head - (col + cw - 1)
		if d < 0 && head >= col {
			d = 0
		}
		col += cw
		if d < 0 || d >= len(sweepColors) {
			run.WriteRune(r)
			continue
		}
		flush()
		c := sweepColors[len(sweepColors)-1-d]
		b.WriteString(base.Foreground(lipgloss.Color(c)).Bold(true).Render(string(r)))
	}
	flush()
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
