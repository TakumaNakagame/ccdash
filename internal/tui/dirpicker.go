package tui

// Directory picker for `n`: a window drawn over the dashboard with a text
// input on top and a list of candidate directories below. Picking a
// candidate substitutes it into the input (descending into it); Enter
// with nothing picked starts claude in whatever the input says, so free
// typing keeps working.

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

// dirCand is one row of the picker list.
type dirCand struct {
	path   string // what gets substituted into the input (trailing /)
	label  string // display text
	recent bool   // from a past session's cwd rather than the listing
}

// openDirPicker seeds the picker from the configured default directory.
func (m *model) openDirPicker() {
	start := strings.TrimSpace(m.settings.NewSessionDir)
	switch {
	case start != "":
	case m.remote.Enabled:
		// The local home dir means nothing on the remote host; the remote
		// shell expands the tilde itself.
		start = "~/"
	default:
		start = "~/"
	}
	if !strings.HasSuffix(start, "/") {
		start += "/"
	}
	m.newSessionBuffer = start
	m.pickSel = -1
	m.pickScroll = 0
	m.editingNewSession = true
}

// closeDirPicker hides the picker and clears its state.
func (m *model) closeDirPicker() {
	m.editingNewSession = false
	m.newSessionBuffer = ""
	m.pickSel = -1
	m.pickScroll = 0
}

// dirCandidates lists what the picker offers for the current input: the
// parent, subdirectories of the input's directory filtered by the partial
// last segment (prefix matches first, then substring, case-insensitive),
// then recently used session directories matching the input.
func (m *model) dirCandidates() []dirCand {
	buf := m.newSessionBuffer
	var out []dirCand
	if !m.remote.Enabled {
		out = append(out, listDirCandidates(buf)...)
	}
	seen := map[string]bool{}
	for _, c := range out {
		seen[strings.TrimSuffix(c.path, "/")] = true
	}
	filter := strings.ToLower(strings.TrimSuffix(tildePath(expandOrSelf(buf)), "/"))
	const maxRecent = 6
	n := 0
	for _, s := range m.recentCwds() {
		if n >= maxRecent {
			break
		}
		disp := tildePath(s)
		if seen[s] || seen[disp] {
			continue
		}
		if filter != "" && filter != "~" && !strings.Contains(strings.ToLower(disp), filter) {
			continue
		}
		out = append(out, dirCand{path: disp + "/", label: disp, recent: true})
		n++
	}
	return out
}

// listDirCandidates reads the directory part of buf from disk.
func listDirCandidates(buf string) []dirCand {
	expanded, err := expandPath(buf)
	if err != nil {
		return nil
	}
	dir, partial := expanded, ""
	if !strings.HasSuffix(buf, "/") {
		dir, partial = filepath.Dir(expanded), filepath.Base(expanded)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	lp := strings.ToLower(partial)
	var prefix, contains []dirCand
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(partial, ".") {
			continue // dotdirs only when explicitly asked for
		}
		c := dirCand{path: tildePath(filepath.Join(dir, name)) + "/", label: name + "/"}
		ln := strings.ToLower(name)
		switch {
		case lp == "" || strings.HasPrefix(ln, lp):
			prefix = append(prefix, c)
		case strings.Contains(ln, lp):
			contains = append(contains, c)
		}
	}
	var out []dirCand
	if parent := filepath.Dir(dir); parent != dir && partial == "" {
		out = append(out, dirCand{path: tildePath(parent) + "/", label: "../"})
	}
	return append(append(out, prefix...), contains...)
}

// recentCwds returns distinct session working directories, most recently
// active first.
func (m *model) recentCwds() []string {
	ss := append([]mdl.Session(nil), m.allSessions...)
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].LastSeen.After(ss[j].LastSeen) })
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if s.Cwd == "" || seen[s.Cwd] || strings.HasPrefix(s.SessionID, "pid-") {
			continue
		}
		seen[s.Cwd] = true
		out = append(out, s.Cwd)
	}
	return out
}

// tildePath shortens a path under $HOME to ~/….
func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// expandOrSelf expands ~ when possible, else returns p unchanged.
func expandOrSelf(p string) string {
	if e, err := expandPath(p); err == nil {
		if strings.HasSuffix(p, "/") && !strings.HasSuffix(e, "/") {
			e += "/"
		}
		return e
	}
	return p
}

// handleKeyNewSessionEdit drives the picker.
func (m *model) handleKeyNewSessionEdit(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	cands := m.dirCandidates()
	if m.pickSel >= len(cands) {
		m.pickSel = len(cands) - 1
	}
	picked := m.pickSel >= 0
	substitute := func() {
		m.newSessionBuffer = cands[m.pickSel].path
		m.pickSel = -1
		m.pickScroll = 0
	}
	move := func(d int) {
		if len(cands) == 0 {
			return
		}
		if m.pickSel < 0 {
			if d > 0 {
				m.pickSel = 0
			} else {
				m.pickSel = len(cands) - 1
			}
			return
		}
		m.pickSel = ((m.pickSel+d)%len(cands) + len(cands)) % len(cands)
	}
	edited := func() { m.pickSel = -1; m.pickScroll = 0 }

	switch key := msg.String(); {
	case msg.Code == tea.KeyEnter:
		if picked {
			substitute()
			return m, nil
		}
		dir := m.newSessionBuffer
		m.closeDirPicker()
		return m, m.startNewSession(dir)
	case msg.Code == tea.KeyEscape || key == "ctrl+c":
		m.closeDirPicker()
		return m, nil
	case key == "down" || key == "ctrl+n":
		move(1)
	case key == "up" || key == "ctrl+p":
		move(-1)
	case key == "tab" || key == "right":
		if !picked {
			move(1)
			if key == "right" && m.pickSel >= 0 {
				substitute()
			}
			return m, nil
		}
		substitute()
	case key == "shift+tab":
		move(-1)
	case key == "left":
		m.newSessionBuffer = parentInput(m.newSessionBuffer)
		edited()
	case key == "ctrl+w" || key == "alt+backspace":
		m.newSessionBuffer = parentInput(m.newSessionBuffer)
		edited()
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.newSessionBuffer); len(r) > 0 {
			m.newSessionBuffer = string(r[:len(r)-1])
		}
		edited()
	case msg.Text != "":
		m.newSessionBuffer += msg.Text
		edited()
	}
	return m, nil
}

// parentInput drops the last path segment: "~/a/b/" and "~/a/b" → "~/a/".
func parentInput(buf string) string {
	t := strings.TrimSuffix(buf, "/")
	i := strings.LastIndex(t, "/")
	if i < 0 {
		return ""
	}
	return t[:i+1]
}

// dirPickerBox renders the picker window and returns it with the cell
// offset of the input caret inside the box (for the IME cursor).
func (m *model) dirPickerBox() (box string, caretX, caretY int) {
	w := m.width - 8
	if w > 90 {
		w = 90
	}
	if w < 30 {
		w = 30
	}
	inner := w - 4 // border + one space padding each side
	maxRows := m.height - 12
	if maxRows > 14 {
		maxRows = 14
	}
	if maxRows < 3 {
		maxRows = 3
	}

	cands := m.dirCandidates()
	if m.pickSel >= len(cands) {
		m.pickSel = len(cands) - 1
	}
	// Keep the highlight in view.
	if m.pickSel >= 0 {
		if m.pickSel < m.pickScroll {
			m.pickScroll = m.pickSel
		}
		if m.pickSel >= m.pickScroll+maxRows {
			m.pickScroll = m.pickSel - maxRows + 1
		}
	}
	if m.pickScroll > len(cands)-maxRows {
		m.pickScroll = max(0, len(cands)-maxRows)
	}

	const promptTxt = "❯ "
	input := m.newSessionBuffer
	// Show the tail of a long input so the caret stays visible.
	for runewidth.StringWidth(promptTxt+input) > inner-1 {
		_, size := utf8.DecodeRuneInString(input)
		input = input[size:]
	}
	lines := []string{
		pendingStyle.Render(promptTxt) + input,
		subtitleStyle.Render(strings.Repeat("-", inner)),
	}
	caretX = 2 + runewidth.StringWidth(promptTxt+input)
	caretY = 1

	end := min(len(cands), m.pickScroll+maxRows)
	lastRecent := false
	for i := m.pickScroll; i < end; i++ {
		c := cands[i]
		if c.recent && !lastRecent {
			lines = append(lines, subtitleStyle.Render("recent"))
		}
		lastRecent = c.recent
		label := c.label
		row := runewidth.Truncate("  "+label, inner, "…")
		if i == m.pickSel {
			row = pendingStyle.Render(runewidth.Truncate("▶ "+label, inner, "…"))
		}
		lines = append(lines, row)
	}
	if len(cands) == 0 {
		lines = append(lines, subtitleStyle.Render("(no matching directories — enter creates it)"))
	}
	if more := len(cands) - end; more > 0 {
		lines = append(lines, subtitleStyle.Render("  …"+strconv.Itoa(more)+" more"))
	}
	var hint string
	if m.pickSel >= 0 {
		hint = "↑↓ select · enter/tab/→ open · ← up · esc cancel"
	} else {
		hint = "↑↓ select · ← up · enter start here · esc cancel"
	}
	lines = append(lines, "", subtitleStyle.Render(runewidth.Truncate(hint, inner, "…")))

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("212")).
		Padding(0, 1).
		Width(w - 2)
	title := " new claude session "
	box = style.Render(strings.Join(lines, "\n"))
	// Put the title into the top border.
	bl := strings.SplitN(box, "\n", 2)
	if len(bl) == 2 && ansi.StringWidth(bl[0]) > len(title)+4 {
		bl[0] = ansi.Cut(bl[0], 0, 2) + pendingStyle.Render(title) + ansi.TruncateLeft(bl[0], 2+len(title), "")
		box = bl[0] + "\n" + bl[1]
	}
	return box, caretX, caretY
}

// overlay draws box over base with its top-left at (x, y), cell-exact.
func overlay(base, box string, x, y int) string {
	rows := strings.Split(base, "\n")
	for i, line := range strings.Split(box, "\n") {
		r := y + i
		if r < 0 || r >= len(rows) {
			continue
		}
		row := rows[r]
		if w := ansi.StringWidth(row); w < x {
			row += strings.Repeat(" ", x-w)
		}
		lw := ansi.StringWidth(line)
		rows[r] = ansi.Cut(row, 0, x) + "\x1b[0m" + line + "\x1b[0m" + ansi.TruncateLeft(row, x+lw, "")
	}
	return strings.Join(rows, "\n")
}
