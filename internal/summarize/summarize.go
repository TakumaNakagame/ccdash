// Package summarize asks the local `claude` CLI to title sessions (ctrl+t).
// We feed claude a digest of each transcript (the human turns and tool
// calls, dropping noisy tool_results / thinking blocks) on stdin and pass
// the instruction prompt as -p, capturing stdout. The spawn is isolated
// (--setting-sources project, cwd = the temp dir) so it never fires
// ccdash's own hooks.
package summarize

import (
	"strings"

	"github.com/takumanakagame/ccmanage/internal/transcript"
)

// Marker prefix that identifies ccdash-spawned `claude -p` invocations.
// We embed it in the instruction so the spawned session's first user
// message starts with it; discovery uses this to keep these throwaway
// sessions out of the dashboard's main list.
const Marker = "[ccdash:summary]"

// buildDigest produces a plain-text rendering of a transcript suitable for
// feeding back into Claude as input. We drop tool_result and thinking blocks
// (huge, low-signal-per-byte) and keep user prompts, assistant text, and a
// one-line summary of each tool call. When the result exceeds budget bytes
// we trim the middle, preserving the first few exchanges (goal context) and
// the latest activity (current state).
func buildDigest(msgs []transcript.Message, budget int) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Kind {
		case transcript.KindUser:
			if m.Text == "" {
				continue
			}
			b.WriteString("USER: ")
			b.WriteString(m.Text)
			b.WriteString("\n\n")
		case transcript.KindAssistant:
			if m.Text == "" {
				continue
			}
			b.WriteString("CLAUDE: ")
			b.WriteString(m.Text)
			b.WriteString("\n\n")
		case transcript.KindToolUse:
			b.WriteString("TOOL ")
			b.WriteString(m.Tool)
			if m.ToolInput != "" {
				b.WriteString(": ")
				b.WriteString(truncate(m.ToolInput, 200))
			}
			b.WriteString("\n")
		}
	}
	full := b.String()
	if len(full) <= budget {
		return full
	}
	// Trim middle: keep first 30% and last 70% of budget.
	head := budget * 30 / 100
	tail := budget - head
	return full[:head] + "\n\n[... transcript trimmed ...]\n\n" + full[len(full)-tail:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
