package plaintext

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	for _, ok := range []string{"hello", "  進み具合を教えてください\n2 行目\tタブ  ", "a /slash inside", "1 - 2"} {
		got, err := Check(ok)
		if err != nil || got != strings.TrimSpace(ok) {
			t.Errorf("Check(%q) = %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{"", "   ", "/clear", "!rm -rf x", "# remember", "-p", "  /compact", "a\x1b[Zb", "a\rb", "a\x00b", "a b", "bell\a",
		strings.Repeat("あ", Max+1)} {
		if _, err := Check(bad); err == nil {
			t.Errorf("Check(%.20q) passed", bad)
		}
	}
	if _, err := Check(strings.Repeat("あ", Max)); err != nil {
		t.Errorf("a text of exactly Max runes: %v", err)
	}
}
