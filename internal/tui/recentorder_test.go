package tui

import (
	"testing"
	"time"

	mdl "github.com/TakumaNakagame/ccdash/internal/model"
)

// Recent order ignores projects, pins and watch-later: one run by latest
// activity (LastSeen when there is none), and only date groups.
func TestRecentOrder(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.Local)
	src := []mdl.Session{
		{Num: 1, Project: "web", Favorite: true, LastSeen: now.Add(-48 * time.Hour)},
		{Num: 2, LastSeen: now.Add(-time.Hour), LastActivity: now.Add(-time.Minute)},
		{Num: 3, Project: "infra", Later: true, LastSeen: now.Add(-30 * time.Minute)},
		{Num: 4, LastSeen: now.Add(-10 * time.Minute), LastActivity: now.Add(-26 * time.Hour)},
	}
	got := byActivity(src)
	var nums []int64
	for _, s := range got {
		nums = append(nums, s.Num)
	}
	if want := []int64{2, 3, 4, 1}; !equalInts(nums, want) {
		t.Fatalf("order = %v, want %v", nums, want)
	}
	if src[0].Num != 1 {
		t.Fatal("byActivity reordered its input")
	}
	for _, c := range []struct {
		s    mdl.Session
		want string
	}{{got[0], "Today"}, {got[1], "Today"}, {got[2], "Yesterday"}, {got[3], "This week"}} {
		if b := bucketFor(c.s, now, true); b != c.want {
			t.Errorf("#%d bucket = %q, want %q", c.s.Num, b, c.want)
		}
	}
	// The project order still groups as before.
	if b := bucketFor(src[2], now, false); b != bucketLater {
		t.Errorf("project order: later bucket = %q", b)
	}
	if b := bucketFor(src[0], now, false); b != projectBucketPrefix+"web" {
		t.Errorf("project order: project bucket = %q", b)
	}
}

func equalInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
