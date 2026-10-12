package tui

import (
	"reflect"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

func TestBeamCells(t *testing.T) {
	for _, n := range []int{1, 7, 40, 300} {
		for tick := -3; tick < 2*beamLoopTicks; tick++ {
			lit := beamCells(tick, n, beamLoopTicks)
			if len(lit) == 0 || len(lit) > n {
				t.Fatalf("n=%d tick=%d: %d lit", n, tick, len(lit))
			}
			for p := range lit {
				if p < 0 || p >= n {
					t.Fatalf("n=%d: cell %d off the perimeter", n, p)
				}
			}
		}
	}
	// One lap per beamLoopTicks.
	a, b := beamCells(3, 100, gridLoopTicks), beamCells(3+gridLoopTicks, 100, gridLoopTicks)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("not periodic")
	}
}

func TestBeamRowKeepsText(t *testing.T) {
	style := lipgloss.NewStyle().Bold(true)
	l1 := fitWidth(style.Render("#3 テストを直してリリース"), 30)
	l2 := fitWidth("Bash: go test ./... · 1m23s", 30)
	for tick := 0; tick < beamLoopTicks; tick++ {
		a, b := beamRow(l1, l2, 30, tick)
		if ansi.Strip(a) != ansi.Strip(l1) || ansi.Strip(b) != ansi.Strip(l2) {
			t.Fatalf("tick %d changed the text:\n%q\n%q", tick, ansi.Strip(a), ansi.Strip(b))
		}
		if ansi.StringWidth(a) != 30 || ansi.StringWidth(b) != 30 {
			t.Fatalf("tick %d: widths %d %d", tick, ansi.StringWidth(a), ansi.StringWidth(b))
		}
	}
}

func TestBeamRowWraps(t *testing.T) {
	const w = 40
	head := beamStops[len(beamStops)-1]
	headAt := func(tick int) int {
		for i, c := range rowBeam(w, tick) {
			if c == head {
				return i
			}
		}
		return -1 // off the row (between passes)
	}
	// Always left to right: wherever the head shows, the next tick has it
	// further right — or gone / back near the left after the right edge.
	sawWrap := false
	for tick := 0; tick < 3*beamLoopTicks; tick++ {
		a, b := headAt(tick), headAt(tick+1)
		switch {
		case a >= 0 && b >= 0 && b < a:
			if b > w/4 {
				t.Fatalf("tick %d: head went back from %d to %d", tick, a, b)
			}
			sawWrap = true
		case a >= 0 && b < 0:
			sawWrap = true // left past the right edge
		}
	}
	if !sawWrap {
		t.Fatal("the band never left the right edge")
	}
}

func TestBeamShade(t *testing.T) {
	if got := beamShade(0, 20); got != beamStops[0] {
		t.Fatalf("tail %s", got)
	}
	if got := beamShade(19, 20); got != beamStops[len(beamStops)-1] {
		t.Fatalf("head %s", got)
	}
	seen := map[string]bool{}
	for r := 0; r < 20; r++ {
		seen[beamShade(r, 20)] = true
	}
	if len(seen) < 16 {
		t.Fatalf("only %d shades over 20 cells", len(seen))
	}
}

func TestWorkLine(t *testing.T) {
	now := time.Now()
	s := mdl.Session{Status: mdl.StatusActive, TurnStartedAt: now.Add(-83 * time.Second), Tool: "Bash", ToolSummary: "go test ./..."}
	if got := workLine(s, now); got != "Bash: go test ./... · 1m23s" {
		t.Fatal(got)
	}
	s.Tool, s.ToolSummary = "", ""
	if got := workLine(s, now); got != "thinking · 1m23s" {
		t.Fatal(got)
	}
	s.TurnStartedAt = time.Time{}
	if got := workLine(s, now); got != "" {
		t.Fatal(got)
	}
	s.Status, s.Tool = mdl.StatusIdle, "Bash"
	if got := workLine(s, now); got != "" {
		t.Fatal(got)
	}
}
