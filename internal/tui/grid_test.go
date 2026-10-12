package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/transcript"
)

func TestGridRects(t *testing.T) {
	for _, tc := range []struct{ n, w, h, cols int }{
		{1, 120, 30, 1}, {2, 120, 30, 2}, {3, 120, 30, 2}, {4, 90, 30, 1}, {6, 200, 30, 3},
	} {
		rs := gridRects(tc.n, tc.w, tc.h)
		if len(rs) != tc.n {
			t.Fatalf("%+v: %d rects", tc, len(rs))
		}
		if cols, _ := gridDims(tc.n, tc.w, tc.h); cols != tc.cols {
			t.Fatalf("%+v: cols %d", tc, cols)
		}
		// Rows fill the width; the last row reaches the bottom.
		last := rs[len(rs)-1]
		if last.y+last.h != tc.h {
			t.Fatalf("%+v: bottom at %d", tc, last.y+last.h)
		}
		if r := rs[min(tc.cols, tc.n)-1]; r.x+r.w != tc.w {
			t.Fatalf("%+v: right edge at %d", tc, r.x+r.w)
		}
	}
}

func gridModel() *model {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 120, 30
	m.settings.WorkAnim = true
	m.sessions = []mdl.Session{
		{SessionID: "a", Num: 1, Title: "テストを直す", Status: mdl.StatusActive, Cwd: "/x/a", Tool: "Bash", ToolSummary: "go test ./..."},
		{SessionID: "b", Num: 2, Title: "quiet", Status: mdl.StatusIdle, Cwd: "/x/b"},
		{SessionID: "c", Num: 3, Title: "asks", Status: mdl.StatusIdle, Cwd: "/x/c", Attention: mdl.AttentionNeedsYou, AttentionReason: "AskUserQuestion"},
	}
	m.allSessions = m.sessions
	m.selSess = 1
	return m
}

func TestGridView(t *testing.T) {
	m := gridModel()
	key := func(s string) {
		m.Update(tea.KeyPressMsg{Code: rune(s[0]), Text: s})
	}

	key("V")
	if !m.gridOpen {
		t.Fatal("V did not open the grid")
	}
	// No marks: the busy sessions — a (working) and c (needs you); the
	// focus moves from the quiet selection to the first tile.
	if got := m.gridSessions(); len(got) != 2 || m.sessions[m.selSess].SessionID != "a" {
		t.Fatalf("tiles %v", got)
	}
	m.gridTails = map[string]*gridTail{"a": {msgs: []transcript.Message{{Kind: transcript.KindAssistant, Text: "running the tests now"}}}}
	out := ansi.Strip(m.View().Content)
	lines := strings.Split(out, "\n")
	if len(lines) > m.height {
		t.Fatalf("%d lines > height %d", len(lines), m.height)
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > m.width {
			t.Fatalf("line wider than the terminal (%d): %q", w, l)
		}
	}
	for _, want := range []string{"#1", "Bash: go test ./...", "running the tests now", "? AskUserQuestion"} {
		if !strings.Contains(out, want) {
			t.Fatalf("grid lacks %q:\n%s", want, out)
		}
	}

	// tab moves the focus = the selection.
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.sessions[m.selSess].SessionID != "c" {
		t.Fatalf("tab: selected %q", m.sessions[m.selSess].SessionID)
	}

	// Marks replace the automatic choice.
	m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if got := m.gridSessions(); len(got) != 1 || m.sessions[got[0]].SessionID != "c" {
		t.Fatalf("after mark: %v", got)
	}

	// esc closes it.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.gridOpen {
		t.Fatal("esc did not close the grid")
	}
}
