package tui

import (
	"testing"
	"time"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

// TestMarkSelectedSeenDwell: passing over a NEW row doesn't clear it; only
// staying on it for seenDwell (with the terminal focused) does.
func TestMarkSelectedSeenDwell(t *testing.T) {
	m := &model{sessions: []mdl.Session{{SessionID: "a", Unseen: true}, {SessionID: "b", Unseen: true}}}
	if m.markSelectedSeen() != nil {
		t.Fatal("first look at a row must not mark it seen")
	}
	m.selSess = 1
	if m.markSelectedSeen() != nil || m.dwellSID != "b" {
		t.Fatal("moving to another row restarts the dwell")
	}
	m.dwellSince = time.Now().Add(-seenDwell)
	m.blurred = true
	if m.markSelectedSeen() != nil {
		t.Fatal("an unfocused terminal marks nothing seen")
	}
	m.blurred = false
	if m.markSelectedSeen() == nil {
		t.Fatal("after the dwell the row becomes seen")
	}
}
