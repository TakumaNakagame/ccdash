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
	in := []mdl.Session{
		{SessionID: "fav", Favorite: true, LastSeen: at(1)},
		{SessionID: "plain", LastSeen: at(0)},
		{SessionID: "a-old", Project: "a", LastSeen: at(30)},
		{SessionID: "b", Project: "b", LastSeen: at(5)},
		{SessionID: "a-new", Project: "a", LastSeen: at(2)},
	}
	var got []string
	for _, s := range projectsFirst(in) {
		got = append(got, s.SessionID)
	}
	want := "a-new a-old b fav plain"
	if strings.Join(got, " ") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
}
