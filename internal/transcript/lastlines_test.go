package transcript

import "testing"

func TestLastLines(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{"a\nb\nc\n", 5, "a\nb\nc\n"},
		{"a\nb\nc\n", 3, "a\nb\nc\n"},
		{"a\n\nb\n\n", 1, "b\n\n"},
		{"a\nb\n", 0, "a\nb\n"},
		{"", 3, ""},
		{"only\n", 1, "only\n"},
	} {
		if got := string(LastLines([]byte(c.in), c.n)); got != c.want {
			t.Errorf("LastLines(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}
