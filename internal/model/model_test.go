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

// TestColorByNum: consecutive sessions never share an automatic color
// within a palette's length.
func TestColorByNum(t *testing.T) {
	seen := map[string]int64{}
	for n := int64(100); n < 100+int64(len(SessionPalette)); n++ {
		c := (Session{SessionID: "x", Num: n}).Color()
		if prev, ok := seen[c]; ok {
			t.Fatalf("#%d and #%d share %s", prev, n, c)
		}
		seen[c] = n
	}
	if got := (Session{SessionID: "x", Num: 1}).Color(); got != SessionPalette[7] {
		t.Errorf("#1 = %s, want %s", got, SessionPalette[7])
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
