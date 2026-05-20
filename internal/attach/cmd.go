package attach

import (
	"os"
	"os/exec"
	"strings"
)

// ViaShell returns an exec.Cmd that invokes claude through the user's
// interactive shell ($SHELL -i -c). This ensures shell functions defined in
// .zshrc/.bashrc — such as wrappers or credential helpers — are available when
// claude starts. Falls back to direct exec.Command("claude") when $SHELL is unset.
func ViaShell(args ...string) *exec.Cmd {
	shell := os.Getenv("SHELL")
	if shell == "" {
		return exec.Command("claude", args...)
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "claude")
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return exec.Command(shell, "-i", "-c", strings.Join(parts, " "))
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SafeEnv returns a copy of the current environment with terminal-emulator-
// specific variables stripped so they are not inherited by the child PTY.
// Variables like VSCODE_INJECTION or WARP_* signal to claude that it is inside
// a specific IDE/terminal, activating code paths that break inside a ccdash PTY.
func SafeEnv() []string {
	stripPfx := []string{"WARP_", "GHOSTTY_", "ITERM_", "KITTY_", "TABBY_", "CLAUDE_CODE_", "VSCODE_"}
	stripKey := map[string]bool{
		"TERM_PROGRAM":         true,
		"TERM_PROGRAM_VERSION": true,
	}
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		key, _, _ := strings.Cut(kv, "=")
		if stripKey[key] {
			continue
		}
		skip := false
		for _, p := range stripPfx {
			if strings.HasPrefix(key, p) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		if key == "TERM" {
			out = append(out, "TERM=xterm-256color")
			continue
		}
		out = append(out, kv)
	}
	return out
}
