package tui

// "! command" lines in Claude's replies (the way Claude asks the operator
// to run something in its bash mode). `e` collects them from the latest
// reply of the selected session and, after an explicit yes, types "!" and
// the command into the hosted claude — the TUI twin of the portal's
// bangLinks / confirmBang (web/app.js).

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/takumanakagame/ccmanage/internal/store"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

var (
	// In prose the space after "!" is required, so "!important" stays text;
	// in code (a fence or an inline span) "!cmd" counts too. Same rules
	// as bangCmd in web/app.js.
	bangProse   = regexp.MustCompile(`^!\s+([^\s!=].*)$`)
	bangCode    = regexp.MustCompile(`^!\s*([^\s!=].*)$`)
	inlineCode  = regexp.MustCompile("`([^`\n]+)`")
	listMarker  = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+`)
	fenceMarker = regexp.MustCompile("^\\s*(```|~~~)")
)

// bangCommands returns the commands of the "! command" lines in a reply,
// in order, without duplicates.
func bangCommands(text string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(cmd string) {
		cmd = strings.TrimSpace(cmd)
		if cmd != "" && !seen[cmd] {
			seen[cmd] = true
			out = append(out, cmd)
		}
	}
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		if fenceMarker.MatchString(line) {
			inFence = !inFence
			continue
		}
		t := strings.TrimSpace(line)
		if inFence {
			if mm := bangCode.FindStringSubmatch(t); mm != nil {
				add(mm[1])
			}
			continue
		}
		found := false
		for _, sp := range inlineCode.FindAllStringSubmatch(t, -1) {
			if mm := bangCode.FindStringSubmatch(strings.TrimSpace(sp[1])); mm != nil {
				add(mm[1])
				found = true
			}
		}
		if found {
			continue
		}
		t = listMarker.ReplaceAllString(t, "")
		if mm := bangProse.FindStringSubmatch(t); mm != nil {
			add(mm[1])
		}
	}
	return out
}

// lastReplyBangs collects the commands from the assistant text after the
// latest user prompt in msgs.
func lastReplyBangs(msgs []transcript.Message) []string {
	start := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Kind == transcript.KindUser && !transcript.IsNoise(msgs[i].Text) {
			start = i + 1
			break
		}
	}
	var b strings.Builder
	for _, msg := range msgs[start:] {
		if msg.Kind == transcript.KindAssistant {
			b.WriteString(msg.Text)
			b.WriteString("\n")
		}
	}
	return bangCommands(b.String())
}

// openBang is the `e` key: find the commands and ask which to run.
func (m *model) openBang() tea.Cmd {
	if !m.settings.AttachEnabled {
		m.flash = "attach is OFF (settings ',')"
		return nil
	}
	if len(m.sessions) == 0 || m.selSess < 0 || m.selSess >= len(m.sessions) {
		return nil
	}
	key := m.desiredLiveKey()
	if key == "" {
		m.flash = "this session isn't running in ccdash — can't type into it"
		return nil
	}
	if m.tailPath != m.sessions[m.selSess].TranscriptPath {
		m.flash = "transcript is still loading"
		return nil
	}
	cmds := lastReplyBangs(m.tailMessages)
	if len(cmds) == 0 {
		m.flash = "no \"! command\" in the latest reply"
		return nil
	}
	m.bangConfirm = true
	m.bangKey = key
	m.bangCmds = cmds
	m.bangSel = 0
	m.modalFocus = 1 // No: a reflexive enter doesn't run anything
	return nil
}

// handleKeyBang: j/k (or 1-9) pick the command when there are several;
// the rest is the usual Yes / No window (resolveModalKey).
func (m *model) handleKeyBang(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if len(m.bangCmds) > 1 {
		switch s := msg.String(); s {
		case "up", "k":
			m.bangSel = (m.bangSel - 1 + len(m.bangCmds)) % len(m.bangCmds)
			return m, nil
		case "down", "j":
			m.bangSel = (m.bangSel + 1) % len(m.bangCmds)
			return m, nil
		default:
			if len(s) == 1 && s[0] >= '1' && s[0] <= '9' && int(s[0]-'1') < len(m.bangCmds) {
				m.bangSel = int(s[0] - '1')
				return m, nil
			}
		}
	}
	action := m.resolveModalKey(msg)
	if action == "" {
		return m, nil
	}
	key, cmd := m.bangKey, m.bangCmds[clamp(m.bangSel, 0, len(m.bangCmds)-1)]
	m.bangConfirm, m.bangKey, m.bangCmds = false, "", nil
	if action != "y" {
		m.flash = "not run"
		return m, nil
	}
	m.flash = "sent: ! " + cmd
	return m, sendBangCmd(m.ctx, m.store, key, cmd)
}

type bangSentMsg struct{ err error }

// sendBangCmd types "!" into claude's empty input (bash mode), then the
// command and Enter.
func sendBangCmd(ctx context.Context, st store.Store, key, cmd string) tea.Cmd {
	return func() tea.Msg {
		if err := st.SendInput(ctx, key, store.Input{Keys: "!"}); err != nil {
			return bangSentMsg{err}
		}
		time.Sleep(250 * time.Millisecond)
		return bangSentMsg{st.SendInput(ctx, key, store.Input{Text: cmd, Submit: true})}
	}
}

// bangBox renders the confirmation window and its buttons (box-relative).
func (m *model) bangBox() (string, []modalButton) {
	w := min(max(m.width-8, 40), 90)
	inner := w - 4
	var lines []string
	if len(m.bangCmds) > 1 {
		lines = append(lines, "Commands in the latest reply:", "")
		for i, c := range m.bangCmds {
			row := runewidth.Truncate(fmt.Sprintf("%d  ! %s", i+1, c), inner-2, "…")
			if i == m.bangSel {
				lines = append(lines, pendingStyle.Render("▶ "+row))
			} else {
				lines = append(lines, "  "+row)
			}
		}
		lines = append(lines, "")
	}
	cmd := m.bangCmds[clamp(m.bangSel, 0, len(m.bangCmds)-1)]
	lines = append(lines, "Run this in the session's claude (bash mode)?", "")
	lines = append(lines, strings.Split(ansi.Wrap("! "+cmd, inner, ""), "\n")...)
	lines = append(lines, "", subtitleStyle.Render("It runs in the session's terminal as is."), "")
	row, spans := buttonRow([]buttonSpec{
		{key: "y", label: "Yes, run it", style: btnDanger},
		{key: "n", label: "No", style: btnPlain},
	}, m.modalFocus)
	btns := placeButtons(spans, len(lines))
	hint := "←/→ select · enter choose · y / n · esc cancel"
	if len(m.bangCmds) > 1 {
		hint = "j/k or 1-9 pick · " + hint
	}
	lines = append(lines, row, "", subtitleStyle.Render(hint))

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("214")).
		Padding(0, 1).
		Width(w - 2)
	const title = " ! command "
	box := style.Render(strings.Join(lines, "\n"))
	bl := strings.SplitN(box, "\n", 2)
	if len(bl) == 2 && ansi.StringWidth(bl[0]) > len(title)+4 {
		bl[0] = ansi.Cut(bl[0], 0, 2) + pendingStyle.Render(title) + ansi.TruncateLeft(bl[0], 2+len(title), "")
		box = bl[0] + "\n" + bl[1]
	}
	return box, btns
}
