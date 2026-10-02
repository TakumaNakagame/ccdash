package tui

// Skill picker for `S`: a window listing the installed Claude Code skills
// and slash commands, filtered as you type. Picking one hands off to the
// `n` directory picker with "/<skill>" parked in spawnPrompt, so the new
// session starts with the skill already invoked — no separate console
// needed for a one-off skill run.
//
// Free text works too: Tab copies the highlighted name into the input so
// arguments can follow ("/code-flow:ship-pr 123"), and Enter with no
// match sends the input as-is (built-ins like /simplify aren't on disk).

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/takumanakagame/ccmanage/internal/skills"
)

// openSkillPicker loads the skill list and shows the picker. In remote
// mode the skills live on the other host, so only free text is offered.
//
// Project skills (<project>/.claude/skills) come first: the projects are
// found from the selected session's directory, the new-session default,
// and recent session directories (and their parents), since a project
// skill only loads for a claude started inside that project.
func (m *model) openSkillPicker() {
	m.skillList = nil
	m.skillTabbed = skills.Skill{}
	if !m.remote.Enabled {
		if home, err := os.UserHomeDir(); err == nil {
			var dirs []string
			if m.selSess >= 0 && m.selSess < len(m.sessions) && m.sessions[m.selSess].Cwd != "" {
				dirs = append(dirs, m.sessions[m.selSess].Cwd)
			}
			if d := strings.TrimSpace(m.settings.NewSessionDir); d != "" {
				if e, err := expandPath(d); err == nil {
					dirs = append(dirs, e)
				}
			}
			dirs = append(dirs, m.recentCwds()...)
			for _, root := range skills.ProjectRoots(dirs, home) {
				m.skillList = append(m.skillList, skills.ListProject(root)...)
			}
			m.skillList = append(m.skillList, skills.List(filepath.Join(home, ".claude"))...)
		}
	}
	m.skillBuffer = ""
	m.skillSel = 0
	m.skillScroll = 0
	m.editingSkill = true
}

func (m *model) closeSkillPicker() {
	m.editingSkill = false
	m.skillBuffer = ""
	m.skillList = nil
	m.skillTabbed = skills.Skill{}
}

// skillCandidates filters the list by the input: name prefix matches
// first, then name substring, then description substring. Once the input
// has a space the operator is typing arguments, so nothing is offered.
func (m *model) skillCandidates() []skills.Skill {
	return filterSkills(m.skillList, m.skillBuffer)
}

func filterSkills(list []skills.Skill, buf string) []skills.Skill {
	q := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(buf), "/"))
	if strings.Contains(strings.TrimLeft(buf, " "), " ") {
		return nil
	}
	if q == "" {
		return list
	}
	var prefix, name, desc []skills.Skill
	for _, s := range list {
		ln := strings.ToLower(s.Name)
		// Match the part after "plugin:" as a prefix too, so "ship"
		// finds "code-flow:ship-pr".
		_, short, _ := strings.Cut(ln, ":")
		switch {
		case strings.HasPrefix(ln, q) || (short != "" && strings.HasPrefix(short, q)):
			prefix = append(prefix, s)
		case strings.Contains(ln, q):
			name = append(name, s)
		case strings.Contains(strings.ToLower(s.Description), q),
			s.Dir != "" && strings.Contains(strings.ToLower(tildePath(s.Dir)), q):
			desc = append(desc, s)
		}
	}
	return append(append(prefix, name...), desc...)
}

// skillPrompt turns free input into the session's first message.
func skillPrompt(buf string) string {
	p := strings.TrimSpace(buf)
	if p != "" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// handleKeySkillEdit drives the picker.
func (m *model) handleKeySkillEdit(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	cands := m.skillCandidates()
	if m.skillSel >= len(cands) {
		m.skillSel = len(cands) - 1
	}
	if m.skillSel < 0 && len(cands) > 0 {
		m.skillSel = 0
	}
	move := func(d int) {
		if len(cands) > 0 {
			m.skillSel = ((m.skillSel+d)%len(cands) + len(cands)) % len(cands)
		}
	}
	edited := func() { m.skillSel, m.skillScroll = 0, 0 }

	switch key := msg.String(); {
	case msg.Code == tea.KeyEnter:
		prompt := skillPrompt(m.skillBuffer)
		dir := ""
		if len(cands) > 0 {
			prompt = "/" + cands[m.skillSel].Name
			dir = cands[m.skillSel].Dir
		} else if t := m.skillTabbed; t.Name != "" && strings.HasPrefix(prompt+" ", "/"+t.Name+" ") {
			dir = t.Dir // Tab'd a project skill, then typed arguments
		}
		if prompt == "" {
			return m, nil
		}
		m.closeSkillPicker()
		m.openDirPicker()
		if dir != "" {
			// A project skill only loads inside its project: start there.
			m.newSessionBuffer = tildePath(dir) + "/"
		}
		m.spawnPrompt = prompt
		return m, nil
	case msg.Code == tea.KeyEscape || key == "ctrl+c":
		m.closeSkillPicker()
		return m, nil
	case key == "down" || key == "ctrl+n":
		move(1)
	case key == "up" || key == "ctrl+p":
		move(-1)
	case key == "tab":
		// Complete the name and drop into argument mode.
		if len(cands) > 0 {
			m.skillTabbed = cands[m.skillSel]
			m.skillBuffer = "/" + cands[m.skillSel].Name + " "
			edited()
		}
	case key == "ctrl+w" || key == "alt+backspace" || key == "ctrl+u":
		m.skillBuffer = ""
		edited()
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.skillBuffer); len(r) > 0 {
			m.skillBuffer = string(r[:len(r)-1])
		}
		edited()
	case msg.Text != "":
		m.skillBuffer += msg.Text
		edited()
	}
	return m, nil
}

// skillPickerBox renders the picker window and returns it with the cell
// offset of the input caret inside the box (for the IME cursor).
func (m *model) skillPickerBox() (box string, caretX, caretY int) {
	w := min(max(m.width-8, 30), 100)
	inner := w - 4
	maxRows := min(max(m.height-12, 3), 14)

	cands := m.skillCandidates()
	if m.skillSel >= len(cands) {
		m.skillSel = len(cands) - 1
	}
	if m.skillSel >= 0 {
		if m.skillSel < m.skillScroll {
			m.skillScroll = m.skillSel
		}
		if m.skillSel >= m.skillScroll+maxRows {
			m.skillScroll = m.skillSel - maxRows + 1
		}
	}
	if m.skillScroll > len(cands)-maxRows {
		m.skillScroll = max(0, len(cands)-maxRows)
	}

	const promptTxt = "❯ "
	input := m.skillBuffer
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

	label := func(s skills.Skill) string {
		if s.Dir != "" {
			return s.Name + " (" + tildePath(s.Dir) + ")"
		}
		return s.Name
	}
	nameW := 0
	for _, s := range cands {
		nameW = max(nameW, runewidth.StringWidth(label(s)))
	}
	nameW = min(nameW, inner/2)
	end := min(len(cands), m.skillScroll+maxRows)
	for i := m.skillScroll; i < end; i++ {
		s := cands[i]
		name := runewidth.FillRight(runewidth.Truncate(label(s), nameW, "…"), nameW)
		desc := runewidth.Truncate(strings.Join(strings.Fields(s.Description), " "), max(inner-nameW-4, 0), "…")
		if i == m.skillSel {
			lines = append(lines, pendingStyle.Render("▶ "+name)+"  "+subtitleStyle.Render(desc))
		} else {
			lines = append(lines, "  "+name+"  "+subtitleStyle.Render(desc))
		}
	}
	switch {
	case len(cands) == 0 && strings.TrimSpace(m.skillBuffer) != "":
		lines = append(lines, subtitleStyle.Render(runewidth.Truncate("enter runs "+skillPrompt(m.skillBuffer), inner, "…")))
	case len(cands) == 0 && m.remote.Enabled:
		lines = append(lines, subtitleStyle.Render("(remote mode: type a /command)"))
	case len(cands) == 0:
		lines = append(lines, subtitleStyle.Render("(no skills found under ~/.claude)"))
	}
	if more := len(cands) - end; more > 0 {
		lines = append(lines, subtitleStyle.Render("  …"+strconv.Itoa(more)+" more"))
	}
	hint := "↑↓ select · tab add args · enter pick dir · esc cancel"
	lines = append(lines, "", subtitleStyle.Render(runewidth.Truncate(hint, inner, "…")))

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("212")).
		Padding(0, 1).
		Width(w - 2)
	const title = " run skill in new session "
	box = style.Render(strings.Join(lines, "\n"))
	bl := strings.SplitN(box, "\n", 2)
	if len(bl) == 2 && ansi.StringWidth(bl[0]) > len(title)+4 {
		bl[0] = ansi.Cut(bl[0], 0, 2) + pendingStyle.Render(title) + ansi.TruncateLeft(bl[0], 2+len(title), "")
		box = bl[0] + "\n" + bl[1]
	}
	return box, caretX, caretY
}
