package server

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// TestStrFilterKeepsTitleOffScreen replays Claude Code's title updates
// ("✳ <summary>", where ✳ = E2 9C B3) into x/vt. Unfiltered, the 0x9C
// ends the OSC early and the summary lands on the screen.
func TestStrFilterKeepsTitleOffScreen(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"osc bel", []string{"ab\x1b]0;✳ 反映のローカル変更状況\x07cd"}, "abcd"},
		{"osc st", []string{"ab\x1b]0;✳ 反映\x1b\\cd"}, "abcd"},
		{"split mid rune", []string{"ab\x1b]0;\xe2\x9c", "\xb3 反映\x07cd"}, "abcd"},
		{"split after esc", []string{"ab\x1b", "]2;✶ 反映\x07cd"}, "abcd"},
		{"ground utf8 untouched", []string{"✳ 反映"}, "✳ 反映"},
		{"csi untouched", []string{"a\x1b[1mb\x1b[0m反映"}, "ab反映"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emu := vt.NewEmulator(40, 3)
			var f strFilter
			for _, c := range tc.chunks {
				_, _ = emu.Write(f.filter([]byte(c)))
			}
			got := strings.TrimRight(strings.Split(ansi.Strip(emu.Render()), "\n")[0], " ")
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
