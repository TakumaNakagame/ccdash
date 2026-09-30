package discovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLastModifiedIgnoresTouch: `claude --resume` touches the transcript
// without adding entries; the session's time must stay at its newest
// entry, and move once a real entry is appended.
func TestLastModifiedIgnoresTouch(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sess-1.jsonl")
	big := strings.Repeat("x", 200<<10) // tail line with no timestamp, > first 64 KiB window
	content := `{"type":"user","sessionId":"sess-1","cwd":"/w","timestamp":"2026-09-01T10:00:00Z","message":{"role":"user","content":"hi"}}` + "\n" +
		`{"type":"assistant","sessionId":"sess-1","timestamp":"2026-09-02T11:30:00Z"}` + "\n" +
		`{"type":"summary","blob":"` + big + `"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now) // the "resume" touch

	want := time.Date(2026, 9, 2, 11, 30, 0, 0, time.UTC)
	got := scanOne(t, base)
	if !got.Equal(want) {
		t.Fatalf("LastModified = %v, want %v (newest entry, not mtime)", got, want)
	}

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"type":"user","sessionId":"sess-1","timestamp":"2026-09-03T09:00:00Z"}` + "\n")
	f.Close()
	want = time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	if got := scanOne(t, base); !got.Equal(want) {
		t.Fatalf("after append LastModified = %v, want %v", got, want)
	}
}

func scanOne(t *testing.T, base string) time.Time {
	t.Helper()
	ds, err := Scan(context.Background(), base)
	if err != nil || len(ds) != 1 {
		t.Fatalf("Scan = %v, %v", ds, err)
	}
	return ds[0].LastModified
}
