// Package summarize asks the local `claude` CLI to compress a session
// transcript into a few bullet points. We feed claude a digest (the human
// turns and tool calls, dropping noisy tool_results / thinking blocks) on
// stdin and pass the instruction prompt as -p, capturing stdout.
package summarize

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/redact"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

// Marker prefix that identifies summary-spawned `claude -p` invocations.
// We embed it in the instruction so the spawned session's first user
// message starts with it; discovery uses this to keep these throwaway
// sessions out of the dashboard's main list.
const Marker = "[ccdash:summary]"

// Run loads the transcript at path, builds a digest, and shells out to
// `claude -p` to produce a 3-5 bullet summary. The spawned process is
// isolated from the user's normal settings so it doesn't inherit ccdash's
// own hooks (and thus doesn't trigger the same approval / event flow that
// the dashboard is observing).
func Run(ctx context.Context, transcriptPath string) (string, error) {
	if transcriptPath == "" {
		return "", fmt.Errorf("no transcript path")
	}
	msgs, err := transcript.Load(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("load transcript: %w", err)
	}
	digest := redact.String(buildDigest(msgs, 32*1024))
	if strings.TrimSpace(digest) == "" {
		return "", fmt.Errorf("transcript is empty")
	}

	instruction := Marker + ` Summarize this Claude Code session in 3-5 short bullets.
Cover (1) what the user is trying to accomplish, (2) the high-level approach
Claude is taking, (3) the current state. Be concise and concrete; reference
file names or commands where it helps clarity. Reply in the same language
as the user's prompts. No preamble, just the bullets.`

	// --setting-sources project + cwd /tmp gives the spawn a clean
	// settings hierarchy: it skips ~/.claude/settings.json (where our
	// hooks live) and looks instead at /tmp/.claude/settings.json which
	// doesn't exist — so the summarizer doesn't fire SessionStart hooks
	// back at our own server.
	cmd := exec.CommandContext(ctx,
		"claude",
		"--setting-sources", "project",
		"-p", instruction,
	)
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader(digest)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
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
