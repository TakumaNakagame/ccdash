package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEntrypoint: discovery reports the newest entrypoint in the
// transcript — a `claude -p` run is "sdk-cli" until someone resumes it
// interactively.
func TestEntrypoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := func(ep string) string {
		return `{"type":"user","sessionId":"s","entrypoint":"` + ep + `","timestamp":"2026-09-01T10:00:00Z","message":{"role":"user","content":"x"}}` + "\n"
	}
	if err := os.WriteFile(path, []byte(line("sdk-cli")+line("sdk-cli")), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := readTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if d.Entrypoint != "sdk-cli" {
		t.Fatalf("entrypoint = %q, want sdk-cli", d.Entrypoint)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(line("cli"))
	f.Close()
	if d, _ = readTranscript(path); d.Entrypoint != "cli" {
		t.Fatalf("after resume: entrypoint = %q, want cli", d.Entrypoint)
	}
}
