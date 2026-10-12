package model

import (
	"testing"
	"time"
)

func TestRecentReply(t *testing.T) {
	now := time.Now()
	base := Session{Status: StatusIdle, LastActivity: now.Add(-5 * time.Minute)}
	cases := []struct {
		name string
		mod  func(*Session)
		want bool
	}{
		{"seen, replied 5m ago", func(*Session) {}, true},
		{"still unread", func(s *Session) { s.Unseen = true }, false},
		{"working", func(s *Session) { s.Status = StatusActive }, false},
		{"needs you", func(s *Session) { s.Attention = AttentionNeedsYou }, false},
		{"pending approval", func(s *Session) { s.PendingCount = 1 }, false},
		{"too old", func(s *Session) { s.LastActivity = now.Add(-RecentReplyWindow - time.Second) }, false},
		{"no activity", func(s *Session) { s.LastActivity = time.Time{} }, false},
	}
	for _, tc := range cases {
		s := base
		tc.mod(&s)
		if got := s.RecentReply(now); got != tc.want {
			t.Errorf("%s: RecentReply = %v, want %v", tc.name, got, tc.want)
		}
	}
}
