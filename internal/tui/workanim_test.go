package tui

import (
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

func TestSweepKeepsText(t *testing.T) {
	for _, text := range []string{"fix the parser", "日本語のタイトル", "a"} {
		for tick := -3; tick < 40; tick++ {
			if got := ansi.Strip(sweep(text, tick, lipgloss.NewStyle())); got != text {
				t.Fatalf("sweep(%q, %d) = %q", text, tick, got)
			}
		}
	}
}

func TestWorkLine(t *testing.T) {
	now := time.Now()
	s := mdl.Session{Status: mdl.StatusActive, TurnStartedAt: now.Add(-83 * time.Second), Tool: "Bash", ToolSummary: "go test ./..."}
	if got := workLine(s, now); got != "Bash: go test ./... · 1m23s" {
		t.Fatal(got)
	}
	s.Tool, s.ToolSummary = "", ""
	if got := workLine(s, now); got != "thinking · 1m23s" {
		t.Fatal(got)
	}
	s.TurnStartedAt = time.Time{}
	if got := workLine(s, now); got != "" {
		t.Fatal(got)
	}
	s.Status, s.Tool = mdl.StatusIdle, "Bash"
	if got := workLine(s, now); got != "" {
		t.Fatal(got)
	}
}
