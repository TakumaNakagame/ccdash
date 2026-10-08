package model

import "testing"

// TestColorStable pins a few IDs; sessionColor in internal/hub/web/app.js
// yields the same values (same hash, same palette).
func TestColorStable(t *testing.T) {
	cases := map[string]string{
		"":                                     "",
		"a":                                    "#3b82f6",
		"abc":                                  "#14b8a6",
		"5c1acbd1-46b8-408b-a841-2847ce1e9912": "#22c55e",
	}
	for id, want := range cases {
		if got := (Session{SessionID: id}).Color(); got != want {
			t.Errorf("Color(%q) = %q, want %q", id, got, want)
		}
	}
}
