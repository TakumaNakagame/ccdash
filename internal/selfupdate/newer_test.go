package selfupdate

import "testing"

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		cur, tag string
		want     bool
	}{
		{"v0.9.56", "v0.9.57-rc.1", true},
		{"v0.9.57-rc.3", "v0.9.57", true},       // promoted to stable
		{"v0.9.57-rc.3", "v0.9.56", false},      // channel switched back to stable
		{"v0.9.57-rc.2", "v0.9.57-rc.3", true},  // next rc
		{"v0.9.57-rc.3", "v0.9.57-rc.3", false}, // up to date
		{"v0.9.57", "", false},
		{"dev", "v0.9.57", true},
	} {
		if got := Newer(tc.cur, tc.tag); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.cur, tc.tag, got, tc.want)
		}
	}
}
