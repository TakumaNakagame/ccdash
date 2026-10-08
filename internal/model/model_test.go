package model

import "testing"

// TestColorStable pins a few IDs; sessionColor in internal/hub/web/app.js
// yields the same values (same hash, same palette).
func TestColorStable(t *testing.T) {
	cases := map[string]string{
		"":                                     "",
		"a":                                    "#84cc16",
		"abc":                                  "#6366f1",
		"5c1acbd1-46b8-408b-a841-2847ce1e9912": "#b45309",
	}
	for id, want := range cases {
		if got := (Session{SessionID: id}).Color(); got != want {
			t.Errorf("Color(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestColorOverride(t *testing.T) {
	if got := (Session{SessionID: "a", ColorOverride: "#123456"}).Color(); got != "#123456" {
		t.Errorf("override ignored: %q", got)
	}
	for c, want := range map[string]bool{"#a1B2c3": true, "#12345": false, "123456#": false, "#12345g": false, "": false} {
		if ValidColor(c) != want {
			t.Errorf("ValidColor(%q) = %v", c, !want)
		}
	}
}
