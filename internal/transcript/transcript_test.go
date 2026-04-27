package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSampleTranscript(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "sample.jsonl")
	const data = `{"type":"permission-mode","permissionMode":"default","sessionId":"x"}
{"type":"system","subtype":"bridge_status","content":"hello","cwd":"/tmp","sessionId":"x","gitBranch":"main"}
{"type":"user","message":{"role":"user","content":"hello world, please ls"},"timestamp":"2026-04-27T12:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Sure, here we go."},{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls -la"}}]},"timestamp":"2026-04-27T12:00:01Z"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"file1\nfile2\nfile3","is_error":false}]},"timestamp":"2026-04-27T12:00:02Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"3 files found."}]},"timestamp":"2026-04-27T12:00:03Z"}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	msgs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	wantKinds := []Kind{KindUser, KindAssistant, KindToolUse, KindToolResult, KindAssistant}
	if len(msgs) != len(wantKinds) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(wantKinds), msgs)
	}
	for i, k := range wantKinds {
		if msgs[i].Kind != k {
			t.Errorf("msg %d kind = %s, want %s", i, msgs[i].Kind, k)
		}
	}
	if msgs[2].Tool != "Bash" {
		t.Errorf("tool name = %q, want Bash", msgs[2].Tool)
	}
	if msgs[2].ToolInput != "ls -la" {
		t.Errorf("tool input summary = %q, want %q", msgs[2].ToolInput, "ls -la")
	}
	if msgs[3].Text == "" {
		t.Errorf("tool result text empty, expected file1...")
	}
}

func TestLoadSkipsCommandPrompts(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cmd.jsonl")
	const data = `{"type":"user","message":{"role":"user","content":"<command-name>review</command-name>"}}
{"type":"user","message":{"role":"user","content":"actual prompt"}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	msgs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 surfaced message, got %d", len(msgs))
	}
	if msgs[0].Text != "actual prompt" {
		t.Errorf("text = %q", msgs[0].Text)
	}
}
