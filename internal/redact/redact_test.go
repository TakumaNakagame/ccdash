package redact

import (
	"strings"
	"testing"
)

func TestStringPatterns(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // substrings that MUST appear after redaction
		none []string // substrings that must NOT appear
	}{
		{
			name: "github PAT",
			in:   "GH_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789",
			want: []string{"REDACTED"},
			none: []string{"ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
		},
		{
			name: "openai key",
			in:   "OPENAI_API_KEY=sk-proj-AAAA1111BBBB2222CCCC3333DDDD4444",
			want: []string{"REDACTED"},
			none: []string{"sk-proj-AAAA1111BBBB2222CCCC3333DDDD4444"},
		},
		{
			name: "bearer header",
			in:   `curl -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIs"`,
			want: []string{"Bearer <REDACTED>"},
			none: []string{"eyJhbGciOiJIUzI1NiIs"},
		},
		{
			name: "url credentials",
			in:   `git clone https://alice:s3cret@github.com/foo/bar`,
			want: []string{"<REDACTED>@github.com"},
			none: []string{"alice:s3cret"},
		},
		{
			name: "aws key id",
			in:   "key=AKIAIOSFODNN7EXAMPLE",
			want: []string{"AKIA<REDACTED>"},
			none: []string{"AKIAIOSFODNN7EXAMPLE"},
		},
		{
			name: "leaves benign text alone",
			in:   "hello world this is a normal log line",
			want: []string{"hello world this is a normal log line"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := String(c.in)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("expected %q in output, got %q", w, got)
				}
			}
			for _, n := range c.none {
				if strings.Contains(got, n) {
					t.Errorf("did NOT expect %q in output, got %q", n, got)
				}
			}
		})
	}
}

func TestJSONRedactsKeysAndValues(t *testing.T) {
	in := []byte(`{"command":"echo $GITHUB_TOKEN","headers":{"Authorization":"Bearer eyJhbGc.payload"},"api_key":"sk-abcdefghijklmnopqrstuvwx"}`)
	out := string(JSON(in))
	for _, sub := range []string{
		"REDACTED",
	} {
		if !strings.Contains(out, sub) {
			t.Errorf("expected %q in %q", sub, out)
		}
	}
	for _, sub := range []string{
		"sk-abcdefghijklmnopqrstuvwx",
		"eyJhbGc.payload",
	} {
		if strings.Contains(out, sub) {
			t.Errorf("did NOT expect %q in %q", sub, out)
		}
	}
}
