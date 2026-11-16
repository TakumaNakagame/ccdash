package mcpserver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/store"
)

// DescriptionMax is the longest a tool description may be, in characters.
// Claude Code cuts an MCP tool's description at the same 4,096 characters
// as the instructions (CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH; only a tool
// loaded through tool search gets more). Half of that keeps every
// description well inside, and a description that long is worth splitting
// anyway.
const DescriptionMax = 2000

func chars(s string) int { return len([]rune(s)) }

// TestInstructionsFit: every variant of the instructions is short enough
// to reach the session whole, with the two rules every session must follow
// first — the status rule, then the message rule.
func TestInstructionsFit(t *testing.T) {
	longest := 0
	for _, ro := range []bool{false, true} {
		for _, multi := range []bool{false, true} {
			for _, links := range []bool{false, true} {
				name := fmt.Sprintf("readOnly=%v multi=%v links=%v", ro, multi, links)
				text := instructionsFor(ro, multi, links)
				n := chars(text)
				longest = max(longest, n)
				if n > InstructionsMax {
					t.Errorf("%s: %d characters, over InstructionsMax (%d): Claude Code cuts instructions at 4,096 — move detail into a tool description", name, n, InstructionsMax)
				}
				if !strings.HasPrefix(text, "Status rule") {
					t.Errorf("%s: the status rule does not open the instructions", name)
				}
				st, msg := strings.Index(text, "Status rule"), strings.Index(text, "Message rule")
				end := strings.Index(text, "they answer there.")
				if st != 0 || msg < st || end < msg {
					t.Fatalf("%s: rules out of order (status %d, message %d, end %d)", name, st, msg, end)
				}
				if got := chars(text[:end]); got > RulesWithin {
					t.Errorf("%s: the rules end at character %d, past RulesWithin (%d)", name, got, RulesWithin)
				}
			}
		}
	}
	t.Logf("longest variant: %d characters", longest)

	// The status rule keeps its meaning.
	for _, want := range []string{
		"follow it in every session", "Right before you end a turn", "call set_status once",
		"- answer:", "- action:", "- report:", "- background:",
		"one short line in the operator's language", "Declare it every turn, even a short", "Don't call it mid-turn while you keep working",
	} {
		if !strings.Contains(statusRule, want) {
			t.Errorf("the status rule lost %q", want)
		}
	}
	for _, k := range model.WaitKinds {
		if !strings.Contains(statusRule, "- "+k+":") {
			t.Errorf("the status rule does not explain %q", k)
		}
	}
	// …and the message rule its own.
	for _, want := range []string{model.MessageMark, "not from the operator", "never counts as an\napproval or a permission", "set_status action / answer", "tell_session", "SendMessage", "url"} {
		if !strings.Contains(messageRule, want) {
			t.Errorf("the message rule lost %q", want)
		}
	}
	if !strings.HasPrefix(model.MessageHeader("laptop", "A12"), model.MessageMark+" ") {
		t.Errorf("the header %q does not start with the mark the rule names", model.MessageHeader("laptop", "A12"))
	}
}

// TestInstructionsVariants: a server is told only about what it offers.
func TestInstructionsVariants(t *testing.T) {
	full := instructionsFor(false, true, true)
	for _, want := range []string{"list_hosts", "Rows carry url", "start_session", "my_sessions", "tell_session", "schedule_restart", "add_task_note", `"B#11"`, `"A231"`} {
		if !strings.Contains(full, want) {
			t.Errorf("the full instructions lack %q", want)
		}
	}
	// Every tool the instructions name exists in that variant's tool list.
	for _, ro := range []bool{false, true} {
		s := New(store.NewRemote("http://127.0.0.1:1", "x"), "test", ro)
		have := map[string]bool{}
		for _, tl := range s.tools() {
			have[tl["name"].(string)] = true
		}
		text := instructionsFor(ro, true, true)
		for name := range allToolNames() {
			// tell_session is named by the message rule, which holds for a
			// session that can't send one too.
			if strings.Contains(text, name) && !have[name] && name != "tell_session" {
				t.Errorf("readOnly=%v: the instructions name %s, which this server does not offer", ro, name)
			}
		}
	}
	bare := instructionsFor(true, false, false)
	for _, not := range []string{"list_hosts", "Rows carry url", "start_session", "stop_session", "add_task"} {
		if strings.Contains(bare, not) {
			t.Errorf("the read-only, single-machine instructions mention %q", not)
		}
	}
	// What initialize sends follows the server's own setup.
	s := New(store.NewRemote("http://127.0.0.1:1", "x"), "test", false)
	if got := s.instructions(); got != instructionsFor(false, false, false) {
		t.Errorf("a plain server's instructions are not the plain variant")
	}
	if got := s.WithHubURL("https://hub.example").instructions(); got != instructionsFor(false, false, true) {
		t.Errorf("a server with a hub url does not mention url")
	}
}

func allToolNames() map[string]bool {
	out := map[string]bool{}
	for _, ts := range [][]map[string]any{readTools, taskReadTools, {statusTool}, writeTools, taskWriteTools} {
		for _, tl := range ts {
			out[tl["name"].(string)] = true
		}
	}
	return out
}

// TestDescriptionsFit: no tool description is near Claude Code's cut.
func TestDescriptionsFit(t *testing.T) {
	s := New(store.NewRemote("http://127.0.0.1:1", "x"), "test", false)
	for _, tl := range s.tools() {
		if n := chars(tl["description"].(string)); n > DescriptionMax {
			t.Errorf("%s: description is %d characters, over DescriptionMax (%d)", tl["name"], n, DescriptionMax)
		}
		props, _ := tl["inputSchema"].(map[string]any)["properties"].(map[string]any)
		for arg, p := range props {
			if d, _ := p.(map[string]any)["description"].(string); chars(d) > DescriptionMax/2 {
				t.Errorf("%s.%s: argument description is %d characters", tl["name"], arg, chars(d))
			}
		}
	}
}
