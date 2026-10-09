package tui

// `?` opens a window listing every dashboard key. The footer only has room
// for the common ones (and fewer while a live pane is shown), so this is
// where the rest stay discoverable.

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

type helpEntry struct{ key, desc string }

type helpSection struct {
	title   string
	entries []helpEntry
}

var helpSections = []helpSection{
	{"Move", []helpEntry{
		{"j/k up/down", "select session"},
		{"g/G", "top / end"},
		{"h/l tab", "previous / next tab"},
		{"J/K pgup/pgdn", "scroll the right pane"},
		{"< / >", "resize the panes"},
		{"/", "search"},
		{"@", "cycle account filter"},
		{"!", "needs-you / unseen only"},
		{"esc", "clear search and filters"},
	}},
	{"Sessions", []helpEntry{
		{"enter", "open (type into claude)"},
		{"ctrl+]", "focus the live pane"},
		{"F", "fullscreen attach"},
		{"n", "new session"},
		{"S", "run a skill in a new session"},
		{"ctrl+r", "restart the hosted claude"},
		{"o", "open the transcript"},
		{"r", "refresh"},
	}},
	{"Approvals", []helpEntry{
		{"a", "allow"},
		{"A", "allow and remember"},
		{"d", "deny"},
	}},
	{"Organize", []helpEntry{
		{"p", "put in a project (empty = remove)"},
		{"P", "new color for the project"},
		{"[ / ]", "move the project up / down"},
		{"t", "rename"},
		{"ctrl+t", "generate titles (claude -p)"},
		{"T", "assign a tab group"},
		{"R", "toggle auto repo tabs"},
		{"f", "favorite"},
		{"c / C", "next color / random color"},
		{"x", "archive / unarchive"},
		{"ctrl+x", "archive the whole tab"},
		{"X", "archive view"},
	}},
	{"App", []helpEntry{
		{",", "settings"},
		{"u", "update (when available)"},
		{"?", "this help"},
		{"q ctrl+c", "quit"},
	}},
}

// helpLines lays the sections out in one or two columns for width. Key
// labels stay ASCII: arrows are East-Asian ambiguous, so padding them
// with runewidth misaligns the column on CJK locales.
func helpLines(width int) []string {
	const keyW = 15
	render := func(sec helpSection, colW int) []string {
		out := []string{titleStyle.Render(sec.title)}
		for _, e := range sec.entries {
			k := runewidth.FillRight(e.key, keyW)
			d := runewidth.Truncate(e.desc, max(colW-keyW-2, 4), "…")
			out = append(out, "  "+pendingStyle.Render(k)+d)
		}
		return append(out, "")
	}
	if width < 84 {
		var out []string
		for _, sec := range helpSections {
			out = append(out, render(sec, width)...)
		}
		return out
	}
	colW := (width - 2) / 2
	var left, right []string
	for i, sec := range helpSections {
		if i < 2 {
			left = append(left, render(sec, colW)...)
		} else {
			right = append(right, render(sec, colW)...)
		}
	}
	var out []string
	for i := 0; i < max(len(left), len(right)); i++ {
		var l, r string
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		out = append(out, l+strings.Repeat(" ", max(colW-lipgloss.Width(l), 0)+2)+r)
	}
	return out
}

// handleKeyHelp: j/k/arrows scroll, anything else closes.
func (m *model) handleKeyHelp(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		m.helpScroll++
	case "k", "up":
		m.helpScroll = max(m.helpScroll-1, 0)
	default:
		m.helpOpen = false
		m.helpScroll = 0
	}
	return m, nil
}

// helpBox renders the help window, clipped to the terminal height.
func (m *model) helpBox() string {
	w := min(max(m.width-6, 40), 110)
	inner := w - 4
	lines := helpLines(inner)
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	room := max(m.height-6, 5)
	more := len(lines) > room
	if more {
		room-- // the scroll hint
		m.helpScroll = min(m.helpScroll, len(lines)-room)
		lines = lines[m.helpScroll : m.helpScroll+room]
		lines = append(lines, subtitleStyle.Render("j/k scroll · any other key closes"))
	} else {
		m.helpScroll = 0
		lines = append(lines, "", subtitleStyle.Render("any key closes"))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(0, 1).
		Width(w - 2).
		Render(strings.Join(lines, "\n"))
}

// fitKeys trims a footer key legend ("k desc  k desc  …", items separated
// by two spaces) to width by dropping items from the middle, so the
// trailing "? help  q quit" stays visible on a narrow terminal.
func fitKeys(keys string, width int) string {
	if width <= 0 || runewidth.StringWidth(keys) <= width {
		return keys
	}
	items := strings.Split(keys, "  ")
	var tail []string
	for i, it := range items {
		if strings.HasPrefix(it, "? ") {
			tail = items[i:]
			items = items[:i]
			break
		}
	}
	out := strings.Join(tail, "  ")
	for len(items) > 0 {
		head := strings.Join(items, "  ")
		cand := head
		if out != "" {
			cand += "  " + out
		}
		if runewidth.StringWidth(cand) <= width {
			return cand
		}
		items = items[:len(items)-1]
	}
	return runewidth.Truncate(out, width, "…")
}
