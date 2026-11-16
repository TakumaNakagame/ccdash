// Package plaintext is the rule for text that one session (or an outside
// caller) hands to another claude: it must reach Claude Code as plain
// words, never as a command, a flag or a keystroke. Shared by
// `ccdash handoff-mcp` (start_session, send_message) and by messages
// between sessions (ccdash mcp's tell_session, the collector's
// POST /api/sessions/{id}/message).
package plaintext

import (
	"fmt"
	"strings"
	"unicode"
)

// Max caps a request / message (runes).
const Max = 8000

// Check refuses what Claude Code's prompt would not take as plain text: a
// leading / (slash command), ! (shell), # (memory) or - (a flag when it is
// claude's argument), and control characters (ESC sequences such as
// shift+tab switch modes; only newline and tab pass). It returns the text
// trimmed.
func Check(t string) (string, error) {
	t = strings.TrimSpace(t)
	if t == "" {
		return "", fmt.Errorf("text is empty")
	}
	if n := len([]rune(t)); n > Max {
		return "", fmt.Errorf("text is too long (%d characters, max %d)", n, Max)
	}
	if strings.ContainsAny(t[:1], "/!#-") {
		return "", fmt.Errorf("text must not start with %q (Claude Code would read it as a command); start with a word", t[:1])
	}
	for _, r := range t {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || r == ' ' || r == ' ') {
			return "", fmt.Errorf("text must not contain control characters")
		}
	}
	return t, nil
}
