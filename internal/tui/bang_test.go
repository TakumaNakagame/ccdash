package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/transcript"
)

func TestBangCommands(t *testing.T) {
	text := strings.Join([]string{
		"Run this to log in:",
		"",
		"! gh auth login",
		"- ! npm install",
		"or `! make test` from the repo root.",
		"!important is not a command, nor is != or !",
		"```sh",
		"!go vet ./...",
		"echo hi",
		"! gh auth login",
		"```",
	}, "\n")
	want := []string{"gh auth login", "npm install", "make test", "go vet ./..."}
	if got := bangCommands(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestLastReplyBangs(t *testing.T) {
	msgs := []transcript.Message{
		{Kind: transcript.KindAssistant, Text: "! old one"},
		{Kind: transcript.KindUser, Text: "next"},
		{Kind: transcript.KindAssistant, Text: "First:"},
		{Kind: transcript.KindToolUse, Tool: "Bash"},
		{Kind: transcript.KindAssistant, Text: "! gcloud auth login"},
	}
	if got := lastReplyBangs(msgs); !reflect.DeepEqual(got, []string{"gcloud auth login"}) {
		t.Fatal(got)
	}
}

func TestBangKey(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 120, 40
	m.settings.AttachEnabled = true
	m.sessions = []mdl.Session{{SessionID: "s1", Title: "t", Status: mdl.StatusIdle, TranscriptPath: "/t.jsonl"}}
	m.tailPath = "/t.jsonl"
	m.tailMessages = []transcript.Message{{Kind: transcript.KindAssistant, Text: "! a\n! b"}}
	key := func(s string) tea.Cmd {
		_, cmd := m.Update(tea.KeyPressMsg{Code: rune(s[0]), Text: s})
		return cmd
	}

	// Not hosted here: nothing to type into.
	key("e")
	if m.bangConfirm {
		t.Fatal("opened for a session not hosted in ccdash")
	}

	m.ptyAlive = map[string]bool{"s1": true}
	key("e")
	if !m.bangConfirm || len(m.bangCmds) != 2 {
		t.Fatalf("confirm=%v cmds=%q", m.bangConfirm, m.bangCmds)
	}
	key("j")
	if box, _ := m.bangBox(); !strings.Contains(ansi.Strip(box), "▶ 2  ! b") {
		t.Fatalf("j did not select b:\n%s", ansi.Strip(box))
	}
	m.View()
	if len(m.modalBtns) != 2 {
		t.Fatalf("buttons=%d", len(m.modalBtns))
	}
	// enter lands on No first.
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil || m.bangConfirm {
		t.Fatalf("enter: cmd=%v confirm=%v", cmd, m.bangConfirm)
	}
	key("e")
	key("2")
	if cmd := key("y"); cmd == nil || m.bangConfirm || m.flash != "sent: ! b" {
		t.Fatalf("y: cmd=%v confirm=%v flash=%q", cmd, m.bangConfirm, m.flash)
	}
}
