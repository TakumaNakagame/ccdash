package attach

import (
	"slices"
	"strings"
	"testing"
)

func TestSafeEnvDropsClaudeSessionVars(t *testing.T) {
	for k, v := range map[string]string{
		"CLAUDECODE":                "1",
		"CLAUDE_PID":                "123",
		"CLAUDE_EFFORT":             "medium",
		"CLAUDE_PROJECT_DIR":        "/x",
		"CLAUDE_CODE_CHILD_SESSION": "1",
		"TERM_PROGRAM":              "vscode",
		"CLAUDE_CONFIG_DIR":         "/cfg",
		"TERM":                      "xterm-ghostty",
	} {
		t.Setenv(k, v)
	}
	env := SafeEnv()
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "CLAUDECODE", "CLAUDE_PID", "CLAUDE_EFFORT", "CLAUDE_PROJECT_DIR", "CLAUDE_CODE_CHILD_SESSION", "TERM_PROGRAM":
			t.Errorf("%s leaked into the child env", k)
		}
	}
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR=/cfg") {
		t.Error("CLAUDE_CONFIG_DIR (operator config) was dropped")
	}
	if !slices.Contains(env, "TERM=xterm-256color") {
		t.Error("TERM not normalized")
	}
}
