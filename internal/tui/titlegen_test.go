package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

func TestRecentTitleCandidates(t *testing.T) {
	now := time.Now()
	m := newModel(context.Background(), nil, RemoteInfo{})
	tp := "/t.jsonl"
	m.sessions = []mdl.Session{
		{SessionID: "old", Num: 1, TranscriptPath: tp, LastSeen: now.Add(-48 * time.Hour)},
		{SessionID: "fresh", Num: 2, TranscriptPath: tp, LastSeen: now.Add(-time.Hour)},
		{SessionID: "renamed", Num: 3, TranscriptPath: tp, LastSeen: now, CustomTitle: "mine"},
		{SessionID: "titled", Num: 4, TranscriptPath: tp, LastSeen: now.Add(-2 * time.Hour), GenTitle: "x", GenTitleAt: now.Add(-time.Hour)},
		{SessionID: "updated", Num: 5, TranscriptPath: tp, LastSeen: now.Add(-time.Minute), GenTitle: "x", GenTitleAt: now.Add(-time.Hour)},
		{SessionID: "running", Num: 6, TranscriptPath: tp, LastSeen: now, TitleStatus: "running"},
		{SessionID: "notranscript", Num: 7, LastSeen: now},
		{SessionID: "placeholder", TranscriptPath: tp, LastSeen: now},
	}
	var got []string
	for _, s := range m.recentTitleCandidates(now) {
		got = append(got, s.SessionID)
	}
	if strings.Join(got, ",") != "updated,fresh" {
		t.Fatalf("candidates = %v, want [updated fresh] (newest first)", got)
	}
}

func TestTitleGenBanner(t *testing.T) {
	now := time.Now()
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.settings.SummaryEnabled = true
	m.sessions = []mdl.Session{
		{SessionID: "a", Num: 12, TranscriptPath: "/t", LastSeen: now},
		{SessionID: "b", Num: 13, TranscriptPath: "/t", LastSeen: now.Add(-time.Minute)},
	}
	m.startTitleGen()
	if !m.awaitTitleGenConfirm || !strings.Contains(m.flash, "#12 only") || !strings.Contains(m.flash, "a = 2 recent") {
		t.Fatalf("banner = %q (await=%v)", m.flash, m.awaitTitleGenConfirm)
	}
	m.handleKeyTitleGenConfirm(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.awaitTitleGenConfirm || m.flash != "title generation cancelled" {
		t.Fatalf("other key should cancel, flash = %q", m.flash)
	}

	m.settings.SummaryEnabled = false
	m.startTitleGen()
	if m.awaitTitleGenConfirm {
		t.Fatal("ctrl+t must respect summary_enabled")
	}
}

func TestRowShowsRefAndGeneratedTitle(t *testing.T) {
	m := newModel(context.Background(), nil, RemoteInfo{})
	s := mdl.Session{SessionID: "abcdef123", Num: 4839, Title: "first prompt", GenTitle: "ccdash 改善", LastSeen: time.Now()}
	row := ansi.Strip(m.renderSessionRow(s, false, 80))
	if !strings.Contains(row, "#4839 ccdash 改善") {
		t.Fatalf("row = %q", row)
	}
	if !sessionMatchesQuery(s, "#4839") || !sessionMatchesQuery(s, "4839") || sessionMatchesQuery(s, "#483") {
		t.Error("search by #N should match exactly")
	}
}
