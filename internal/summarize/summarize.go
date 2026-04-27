// Package summarize asks the local `claude` CLI to compress a session
// transcript into a few bullet points. We feed claude a digest (the human
// turns and tool calls, dropping noisy tool_results / thinking blocks) on
// stdin and pass the instruction prompt as -p, capturing stdout.
package summarize

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/transcript"
)

// Run loads the transcript at path, builds a digest, and shells out to
// `claude -p` to produce a 3-5 bullet summary. The returned string is
// already trimmed; an empty result is returned with a non-nil error if
// claude failed.
func Run(ctx context.Context, transcriptPath string) (string, error) {
	if transcriptPath == "" {
		return "", fmt.Errorf("no transcript path")
	}
	msgs, err := transcript.Load(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("load transcript: %w", err)
	}
	digest := buildDigest(msgs, 32*1024) // ~32KB cap keeps prompt comfortable
	if strings.TrimSpace(digest) == "" {
		return "", fmt.Errorf("transcript is empty")
	}

	const instruction = `Summarize this Claude Code session in 3-5 short bullets.
Cover (1) what the user is trying to accomplish, (2) the high-level approach
Claude is taking, (3) the current state. Be concise and concrete; reference
file names or commands where it helps clarity. Reply in the same language
as the user's prompts. No preamble, just the bullets.`

	cmd := exec.CommandContext(ctx, "claude", "-p", instruction)
	cmd.Stdin = strings.NewReader(digest)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Surface the first stderr line — claude usually explains there.
		hint := strings.TrimSpace(stderr.String())
		if i := strings.IndexByte(hint, '\n'); i > 0 {
			hint = hint[:i]
		}
		if hint == "" {
			return "", fmt.Errorf("claude -p failed: %w", err)
		}
		return "", fmt.Errorf("claude -p failed: %w (%s)", err, hint)
	}
	return strings.TrimSpace(stdout.String()), nil
}

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

// SummaryAge formats a friendly "x ago" string for display next to a cached
// summary timestamp.
func SummaryAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
