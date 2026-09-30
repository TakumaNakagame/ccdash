package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestSelectionText(t *testing.T) {
	rows := []string{
		"hello \x1b[1mworld\x1b[0m    ",
		"日本語テキスト      ",
		"last line here      ",
	}
	cases := []struct {
		name string
		sel  liveSelection
		want string
	}{
		{"single row", liveSelection{ax: 6, ay: 0, bx: 10, by: 0}, "world"},
		{"reversed drag", liveSelection{ax: 10, ay: 0, bx: 6, by: 0}, "world"},
		{"wide chars", liveSelection{ax: 0, ay: 1, bx: 5, by: 1}, "日本語"},
		{"multi row", liveSelection{ax: 6, ay: 0, bx: 3, by: 2}, "world\n日本語テキスト\nlast"},
		{"bottom-up multi row", liveSelection{ax: 3, ay: 2, bx: 6, by: 0}, "world\n日本語テキスト\nlast"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectionText(tc.sel, rows, 20); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHighlightRowKeepsWidth(t *testing.T) {
	row := "ab\x1b[31m日本\x1b[0mcd  "
	got := highlightRow(row, 2, 6)
	if w := ansi.StringWidth(got); w != ansi.StringWidth(row) {
		t.Fatalf("width changed: %d -> %d", ansi.StringWidth(row), w)
	}
	if plain := ansi.Strip(got); plain != "ab日本cd  " {
		t.Fatalf("text changed: %q", plain)
	}
}
