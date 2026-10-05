package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSummarizeSubagent(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "agent-a1.jsonl")
	_ = os.WriteFile(running, []byte(
		`{"type":"user","timestamp":"2026-10-05T00:00:00Z","isSidechain":true,"message":{"role":"user","content":"look around"}}`+"\n"+
			`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"Checking the repo"},{"type":"tool_use","name":"Bash","input":{"command":"ls -la\nmore"}}]}}`+"\n"), 0o600)
	var info subagentInfo
	info.UpdatedAt = time.Now()
	summarizeSubagent(running, &info)
	if info.Status != "running" || info.Activity != "Bash(ls -la)" || info.Tools != 1 || info.StartedAt.IsZero() {
		t.Errorf("running = %+v", info)
	}

	done := filepath.Join(dir, "agent-a2.jsonl")
	_ = os.WriteFile(done, []byte(
		`{"type":"user","timestamp":"2026-10-05T00:00:00Z","message":{"role":"user","content":"go"}}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/x"}}]}}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"SubagentHandback","input":{"message":"All done: 3 findings"}}]}}`+"\n"), 0o600)
	info = subagentInfo{UpdatedAt: time.Now()}
	summarizeSubagent(done, &info)
	if info.Status != "done" || info.Report != "All done: 3 findings" || info.Tools != 1 {
		t.Errorf("done = %+v", info)
	}

	info = subagentInfo{UpdatedAt: time.Now().Add(-time.Hour)}
	summarizeSubagent(running, &info)
	if info.Status != "stale" {
		t.Errorf("old unfinished = %q, want stale", info.Status)
	}
}
