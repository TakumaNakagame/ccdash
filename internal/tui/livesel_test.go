package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

func TestSelectionText(t *testing.T) {
	rows := []string{
		"hello \x1b[1mworld\x1b[0m    ",
		"日本語テキスト      ",
		"last line here      ",
	}
	cases := []struct {
		name string
		sel  liveSelection
		want string
	}{
		{"single row", liveSelection{ax: 6, ay: 0, bx: 10, by: 0}, "world"},
		{"reversed drag", liveSelection{ax: 10, ay: 0, bx: 6, by: 0}, "world"},
		{"wide chars", liveSelection{ax: 0, ay: 1, bx: 5, by: 1}, "日本語"},
		{"multi row", liveSelection{ax: 6, ay: 0, bx: 3, by: 2}, "world\n日本語テキスト\nlast"},
		{"bottom-up multi row", liveSelection{ax: 3, ay: 2, bx: 6, by: 0}, "world\n日本語テキスト\nlast"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectionText(tc.sel, rows, 20); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHighlightRowKeepsWidth(t *testing.T) {
	row := "ab\x1b[31m日本\x1b[0mcd  "
	got := highlightRow(row, 2, 6)
	if w := ansi.StringWidth(got); w != ansi.StringWidth(row) {
		t.Fatalf("width changed: %d -> %d", ansi.StringWidth(row), w)
	}
	if plain := ansi.Strip(got); plain != "ab日本cd  " {
		t.Fatalf("text changed: %q", plain)
	}
}

// TestLivePlaceholderWhileDialing: moving onto a hosted session must keep
// the live layout (cached screen if any) until its stream connects,
// never flash the transcript view.
func TestLivePlaceholderWhileDialing(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.settings.AttachEnabled = true
	m.width, m.height = 160, 40
	m.sessions = []mdl.Session{{SessionID: "aaa"}, {SessionID: "bbb"}}
	m.ptyAlive = map[string]bool{"aaa": true, "bbb": true}

	m.selSess = 1
	out := ansi.Strip(m.renderEventsList(80, 20))
	if !strings.Contains(out, "connecting") || strings.Contains(out, "transcript") {
		t.Fatalf("uncached: want connecting placeholder, got:\n%s", out)
	}

	m.liveCache["aaa"] = &liveScreen{key: "aaa", rows: []string{"cached screen"}, w: 13, h: 1}
	m.selSess = 0
	out = ansi.Strip(m.renderEventsList(80, 20))
	if !strings.Contains(out, "cached screen") {
		t.Fatalf("cached: want cached rows, got:\n%s", out)
	}
}

// TestArchiveKeepsCursorRow: after x drops the selected session from the
// list, the cursor stays on the same row instead of jumping to the
// newest end (the bottom with NewestAtBottom).
func TestArchiveKeepsCursorRow(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.settings.NewestAtBottom = true
	all := []mdl.Session{{SessionID: "a"}, {SessionID: "b"}, {SessionID: "c"}, {SessionID: "d"}}
	// DB order is newest first (a..d); NewestAtBottom renders rows d,c,b,a.
	m.Update(sessionsMsg(all))
	if got := m.sessions[0].SessionID; got != "d" {
		t.Fatalf("row 0 = %q, want d (newest at bottom)", got)
	}
	m.applyGroupFilter() // must not flip the order back
	if got := m.sessions[0].SessionID; got != "d" {
		t.Fatalf("after re-filter row 0 = %q, want d", got)
	}
	m.selSess = 1 // "c"
	_ = m.toggleArchiveCurrent()
	m.Update(sessionsMsg([]mdl.Session{all[0], all[1], all[3]}))
	if got := m.currentSessionID(); got != "b" {
		t.Fatalf("cursor on %q, want %q (same row)", got, "b")
	}
}

// TestSelectSpawnedByTag: after `n`, the new session's row (carrying the
// server's hook tag as WrapperPID) gets the cursor — switching tabs if
// the current one hides it — and is queued for live focus.
func TestSelectSpawnedByTag(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.Update(sessionsMsg([]mdl.Session{
		{SessionID: "old1", UserGroup: "g1"},
		{SessionID: "old2", UserGroup: "g1"},
	}))
	m.groupFilter = "g1"
	m.applyGroupFilter()
	m.Update(ptyStartedMsg{ptyKey: "pid-1", cwd: "/tmp/x", live: true, tag: 1<<30 + 7})
	if m.spawnTag == 0 {
		t.Fatal("spawnTag not recorded")
	}
	// Row not there yet: nothing moves.
	m.Update(sessionsMsg([]mdl.Session{{SessionID: "old1", UserGroup: "g1"}, {SessionID: "old2", UserGroup: "g1"}}))
	if m.spawnTag == 0 {
		t.Fatal("spawnTag consumed before the row appeared")
	}
	m.Update(sessionsMsg([]mdl.Session{
		{SessionID: "new", UserGroup: "g2", WrapperPID: 1<<30 + 7},
		{SessionID: "old1", UserGroup: "g1"},
		{SessionID: "old2", UserGroup: "g1"},
	}))
	if got := m.currentSessionID(); got != "new" {
		t.Fatalf("cursor on %q, want new", got)
	}
	if m.groupFilter != "g2" || m.liveFocusPending != "new" || m.spawnTag != 0 {
		t.Fatalf("group=%q focusPending=%q spawnTag=%d", m.groupFilter, m.liveFocusPending, m.spawnTag)
	}
}
