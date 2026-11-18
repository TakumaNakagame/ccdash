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

// TestSafeEnvDropsRole: an orchestrator's role is the collector's to hand
// out per child; one inherited from the collector's own environment (a
// collector restarted from an orchestrator's shell) must not reach the
// sessions it hosts.
func TestSafeEnvDropsRole(t *testing.T) {
	t.Setenv("CCDASH_ROLE", "orchestrator")
	t.Setenv("CCDASH_ROLE_PROJECT", "web")
	t.Setenv("CCDASH_OTHER", "kept")
	env := SafeEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "CCDASH_ROLE") {
			t.Errorf("%s leaked into the child env", kv)
		}
	}
	if !slices.Contains(env, "CCDASH_OTHER=kept") {
		t.Error("an unrelated CCDASH_ variable was dropped")
	}
}
