package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

func TestFitKeysKeepsHelp(t *testing.T) {
	keys := "↑/↓ sel  h/l tabs  / search  enter open  p project  ? help  q quit"
	if got := fitKeys(keys, 200); got != keys {
		t.Fatalf("wide: %q", got)
	}
	got := fitKeys(keys, 40)
	if runewidth.StringWidth(got) > 40 || !strings.HasSuffix(got, "? help  q quit") {
		t.Fatalf("narrow: %q", got)
	}
}

func TestHelpListsProjectAndTitleKeys(t *testing.T) {
	all := strings.Join(helpLines(100), "\n")
	for _, k := range []string{"ctrl+t", "project", "rename", "archive"} {
		if !strings.Contains(all, k) {
			t.Errorf("help is missing %q", k)
		}
	}
}

func TestProjectsFirst(t *testing.T) {
	now := time.Now()
	at := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	// Activity (LastSeen) must not matter: project a is newer than b,
	// a-new started after a-old.
	in := []mdl.Session{
		{SessionID: "fav", Favorite: true, LastSeen: at(1)},
		{SessionID: "plain", LastSeen: at(0)},
		{SessionID: "a-old", Project: "a", ProjectOrder: -2, FirstSeen: at(40), LastSeen: at(0)},
		{SessionID: "b", Project: "b", ProjectOrder: 5, FirstSeen: at(50), LastSeen: at(0)},
		{SessionID: "a-new", Project: "a", ProjectOrder: -2, FirstSeen: at(30), LastSeen: at(9)},
	}
	var got []string
	for _, s := range projectsFirst(in) {
		got = append(got, s.SessionID)
	}
	want := "a-new a-old b fav plain"
	// A pin in a project leads its block whatever its age; the later pin first.
	in = append(in,
		mdl.Session{SessionID: "a-pin1", Project: "a", ProjectOrder: -2, Favorite: true, PinnedAt: at(5), FirstSeen: at(90)},
		mdl.Session{SessionID: "a-pin2", Project: "a", ProjectOrder: -2, Favorite: true, PinnedAt: at(1), FirstSeen: at(99)})
	got2 := []string{}
	for _, s := range projectsFirst(in) {
		got2 = append(got2, s.SessionID)
	}
	if w := "a-pin2 a-pin1 a-new a-old b fav plain"; strings.Join(got2, " ") != w {
		t.Fatalf("pinned order = %v, want %s", got2, w)
	}
	if strings.Join(got, " ") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
}

func TestProjectCandidates(t *testing.T) {
	m := &model{
		sessions:     []mdl.Session{{SessionID: "a", Project: "web"}},
		allSessions:  []mdl.Session{{SessionID: "a", Project: "web"}, {SessionID: "b", Project: "infra"}},
		projectCands: []mdl.Project{{Name: "archived-only"}, {Name: "web"}},
		groupCandIdx: -1,
	}
	got := strings.Join(m.filteredProjectCandidates(), ",")
	if want := "archived-only,infra," + removeProjectLabel("web"); got != want {
		t.Fatalf("candidates = %q, want %q", got, want)
	}
	m.titleBuffer = "INF"
	if got := strings.Join(m.filteredProjectCandidates(), ","); got != "infra" {
		t.Fatalf("filtered = %q", got)
	}
	// An empty input is "no change", not "remove".
	m.titleBuffer = ""
	if cmd := m.commitProjectEdit(); cmd != nil {
		t.Fatal("empty input should not touch the project")
	}
}
