package tui

// Clickable buttons for the confirmation windows (restart, already
// running). A button is just a key in disguise: the box renderer reports
// where each one landed, View translates that to screen cells, and a left
// click inside one replays its key through the modal's key handler.

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// modalButton is a clickable span: cells [x0, x1) on row y. Coordinates
// are box-relative from the renderer and screen-absolute in m.modalBtns.
type modalButton struct {
	x0, x1, y int
	key       string // replayed as a key press, e.g. "y"
}

type buttonSpec struct {
	key, label string
	style      lipgloss.Style
}

var (
	btnDanger = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(lipgloss.Color("160")).Bold(true)
	btnGo     = lipgloss.NewStyle().Foreground(lipgloss.Color("16")).Background(lipgloss.Color("214")).Bold(true)
	btnPlain  = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(lipgloss.Color("240"))
)

// buttonRow renders the buttons on one line, three spaces apart, and
// their column spans relative to the line start.
func buttonRow(specs []buttonSpec) (string, []modalButton) {
	var parts []string
	var spans []modalButton
	x := 0
	for i, b := range specs {
		if i > 0 {
			parts = append(parts, "   ")
			x += 3
		}
		txt := " " + b.key + "  " + b.label + " "
		w := ansi.StringWidth(txt)
		parts = append(parts, b.style.Render(txt))
		spans = append(spans, modalButton{x0: x, x1: x + w, key: b.key})
		x += w
	}
	return strings.Join(parts, ""), spans
}

// placeButtons turns a button row's spans into box-relative cells, given
// the row's index among the box's content lines. The boxes use a 1-cell
// border and 1 column of padding.
func placeButtons(spans []modalButton, line int) []modalButton {
	out := make([]modalButton, len(spans))
	for i, s := range spans {
		out[i] = modalButton{x0: s.x0 + 2, x1: s.x1 + 2, y: line + 1, key: s.key}
	}
	return out
}

// clickModalButton replays the key of the button under a left click, if
// any. ok reports whether a modal with buttons is showing (so the click
// shouldn't fall through to the dashboard even when it misses).
func (m *model) clickModalButton(mm tea.Mouse) (cmd tea.Cmd, ok bool) {
	if !m.dupConfirm && !m.restartConfirm {
		return nil, false
	}
	for _, b := range m.modalBtns {
		if mm.Y == b.y && mm.X >= b.x0 && mm.X < b.x1 {
			_, cmd := m.handleKey(tea.KeyPressMsg{Code: rune(b.key[0]), Text: b.key})
			return cmd, true
		}
	}
	return nil, true
}
