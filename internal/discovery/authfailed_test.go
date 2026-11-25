package discovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAuthFailedAt: a turn that ended on Claude Code's login error
// ("Login expired · Please run /login") marks the session, and the next
// prompt clears it.
func TestAuthFailedAt(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sess-1.jsonl")
	content := `{"type":"user","sessionId":"sess-1","cwd":"/w","timestamp":"2026-11-25T00:06:00Z","message":{"role":"user","content":"copy the files"}}` + "\n" +
		`{"type":"assistant","sessionId":"sess-1","timestamp":"2026-11-25T00:06:01Z","message":{"role":"assistant","content":[{"type":"text","text":"Login expired · Please run /login"}]},"error":"authentication_failed","isApiErrorMessage":true}` + "\n" +
		`{"type":"system","subtype":"turn_duration","timestamp":"2026-11-25T00:06:02Z"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := scanAll(t, base)
	want := time.Date(2026, 11, 25, 0, 6, 1, 0, time.UTC)
	if !got.AuthFailedAt.Equal(want) {
		t.Fatalf("AuthFailedAt = %v, want %v", got.AuthFailedAt, want)
	}

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"type":"user","sessionId":"sess-1","timestamp":"2026-11-25T01:00:00Z","message":{"role":"user","content":"go on"}}` + "\n")
	f.Close()
	if got := scanAll(t, base); !got.AuthFailedAt.IsZero() {
		t.Fatalf("AuthFailedAt = %v after a new prompt, want zero", got.AuthFailedAt)
	}
}

func scanAll(t *testing.T, base string) Discovered {
	t.Helper()
	ds, err := Scan(context.Background(), base)
	if err != nil || len(ds) != 1 {
		t.Fatalf("Scan = %v, %v", ds, err)
	}
	return ds[0]
}
