package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/takumanakagame/ccmanage/internal/skills"
)

func TestFilterSkills(t *testing.T) {
	list := []skills.Skill{
		{Name: "code-flow:review-pr"},
		{Name: "code-flow:ship-pr"},
		{Name: "pdf", Description: "work with PDF files"},
		{Name: "sre-hub:whoami"},
	}
	names := func(ss []skills.Skill) (out []string) {
		for _, s := range ss {
			out = append(out, s.Name)
		}
		return out
	}
	cases := []struct {
		buf  string
		want []string
	}{
		{"", []string{"code-flow:review-pr", "code-flow:ship-pr", "pdf", "sre-hub:whoami"}},
		{"/ship", []string{"code-flow:ship-pr"}},                     // prefix of the part after "plugin:"
		{"pr", []string{"code-flow:review-pr", "code-flow:ship-pr"}}, // substring
		{"files", []string{"pdf"}},                                   // description
		{"/code-flow:ship-pr 123", nil},                              // argument mode
	}
	for _, c := range cases {
		got := names(filterSkills(list, c.buf))
		if len(got) != len(c.want) {
			t.Errorf("%q: got %v, want %v", c.buf, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: got %v, want %v", c.buf, got, c.want)
				break
			}
		}
	}
}

// TestSkillPickerHandsOffToDirPicker: S → type → Enter parks "/<skill>"
// for the directory picker; Tab + args + Enter sends the typed command;
// esc in the directory picker forgets the prompt.
func TestSkillPickerHandsOffToDirPicker(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.settings.AttachEnabled = true
	press := func(k tea.KeyPressMsg) { m.Update(k) }
	typeText := func(s string) {
		for _, r := range s {
			press(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}

	press(tea.KeyPressMsg{Code: 'S', Text: "S"})
	if !m.editingSkill {
		t.Fatal("S did not open the skill picker")
	}
	m.skillList = []skills.Skill{{Name: "code-flow:review-pr"}, {Name: "code-flow:ship-pr"}}
	typeText("ship")
	press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.editingSkill || !m.editingNewSession || m.spawnPrompt != "/code-flow:ship-pr" {
		t.Fatalf("skill=%v dir=%v prompt=%q", m.editingSkill, m.editingNewSession, m.spawnPrompt)
	}
	press(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.editingNewSession || m.spawnPrompt != "" {
		t.Fatalf("esc: dir=%v prompt=%q", m.editingNewSession, m.spawnPrompt)
	}

	press(tea.KeyPressMsg{Code: 'S', Text: "S"})
	m.skillList = []skills.Skill{{Name: "code-flow:ship-pr"}}
	press(tea.KeyPressMsg{Code: tea.KeyTab})
	typeText("123")
	press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.spawnPrompt != "/code-flow:ship-pr 123" {
		t.Fatalf("prompt = %q", m.spawnPrompt)
	}
}

// TestProjectSkillStartsInProject: picking a project skill (directly or
// via Tab + arguments) seeds the directory picker with its project.
func TestProjectSkillStartsInProject(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.settings.AttachEnabled = true
	press := func(k tea.KeyPressMsg) { m.Update(k) }
	typeText := func(s string) {
		for _, r := range s {
			press(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
	list := []skills.Skill{
		{Name: "work-status-check", Source: "project", Dir: "/w/working"},
		{Name: "code-flow:ship-pr", Source: "plugin"},
	}

	press(tea.KeyPressMsg{Code: 'S', Text: "S"})
	m.skillList = list
	typeText("work")
	press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.spawnPrompt != "/work-status-check" || m.newSessionBuffer != "/w/working/" {
		t.Fatalf("prompt=%q dir=%q", m.spawnPrompt, m.newSessionBuffer)
	}
	press(tea.KeyPressMsg{Code: tea.KeyEscape})

	press(tea.KeyPressMsg{Code: 'S', Text: "S"})
	m.skillList = list
	press(tea.KeyPressMsg{Code: tea.KeyTab})
	typeText("--since 2026-09-01")
	press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.spawnPrompt != "/work-status-check --since 2026-09-01" || m.newSessionBuffer != "/w/working/" {
		t.Fatalf("tab+args: prompt=%q dir=%q", m.spawnPrompt, m.newSessionBuffer)
	}
	press(tea.KeyPressMsg{Code: tea.KeyEscape})

	// A user/plugin skill keeps the configured default directory.
	press(tea.KeyPressMsg{Code: 'S', Text: "S"})
	m.skillList = list
	typeText("ship")
	press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.newSessionBuffer == "/w/working/" {
		t.Fatalf("plugin skill jumped into the project dir")
	}
}
