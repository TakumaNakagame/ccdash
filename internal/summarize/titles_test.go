package summarize

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseTitles(t *testing.T) {
	out := "Here you go:\n```json\n[{\"id\": 3, \"title\": \"\\\"ccdash 改善\\\"\"}, {\"id\": 7, \"title\": \"#7 Fix login\\ntest\"}, {\"id\": 0, \"title\": \"x\"}]\n```"
	got, err := parseTitles(out)
	if err != nil {
		t.Fatal(err)
	}
	if got[3] != "ccdash 改善" || got[7] != "Fix login test" || len(got) != 2 {
		t.Fatalf("parseTitles = %v", got)
	}
	if _, err := parseTitles("sorry, no"); err == nil {
		t.Fatal("want an error without a JSON array")
	}
}

func TestCleanTitleBoundsWidth(t *testing.T) {
	long := "とても長いタイトルとても長いタイトルとても長いタイトルとても長いタイトル"
	if got := cleanTitle(long); len([]rune(got)) > maxTitleWidth/2+1 {
		t.Fatalf("cleanTitle kept %q", got)
	}
}

// fakeClaude puts a `claude` on PATH that answers one title per
// "=== SESSION <id>" header it sees on stdin.
func fakeClaude(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
ids=$(grep -o '=== SESSION [0-9]*' | awk '{print $3}')
printf '['; sep=''
for i in $ids; do printf '%s{"id":%s,"title":"title %s"}' "$sep" "$i" "$i"; sep=','; done
printf ']'
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeTranscript(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	line := `{"type":"user","message":{"role":"user","content":"` + text + `"},"timestamp":"2026-04-27T12:00:00Z"}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKickoffTitlesBatch(t *testing.T) {
	fakeClaude(t)
	d := newTestDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", writeTranscript(t, "improve ccdash"))
	insertSession(t, d, "s2", writeTranscript(t, "fix the login test"))
	insertSession(t, d, "s3", "") // no transcript: skipped

	if err := KickoffTitles(ctx, d, []string{"s1", "s2", "s3", "missing"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		a, _, _ := d.GetSession(ctx, "s1")
		b, _, _ := d.GetSession(ctx, "s2")
		if a.TitleStatus == "done" && b.TitleStatus == "done" {
			if a.GenTitle != "title 1" || b.GenTitle != "title 2" {
				t.Fatalf("titles = %q, %q", a.GenTitle, b.GenTitle)
			}
			break
		}
		if a.TitleStatus == "error" || b.TitleStatus == "error" || time.Now().After(deadline) {
			t.Fatalf("status = %q / %q", a.TitleStatus, b.TitleStatus)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if c, _, _ := d.GetSession(ctx, "s3"); c.TitleStatus != "" {
		t.Errorf("transcript-less session touched: %q", c.TitleStatus)
	}
}

func TestKickoffTitlesGateAndEmpty(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "")
	if err := KickoffTitles(ctx, d, []string{"s1"}); !errors.Is(err, ErrNoSessions) {
		t.Fatalf("no transcript: err = %v, want ErrNoSessions", err)
	}
	if err := d.SetSetting(ctx, "summary_enabled", "0"); err != nil {
		t.Fatal(err)
	}
	if err := KickoffTitles(ctx, d, []string{"s1"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("gated: err = %v, want ErrDisabled", err)
	}
}
