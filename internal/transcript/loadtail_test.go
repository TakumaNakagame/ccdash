package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadTailSkipsBoundaryFragment writes a JSONL with a known prefix of
// long records, asks LoadTail for a tiny budget, and confirms we returned
// only the records contained entirely in the trailing slice.
func TestLoadTailSkipsBoundaryFragment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tail.jsonl")

	var data []byte
	// 50 records, each ~200 bytes
	for i := 0; i < 50; i++ {
		line := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"prompt %02d %s"},"timestamp":"2026-04-27T12:%02d:00Z"}`, i, padding(150), i)
		data = append(data, []byte(line)...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// Budget that lands mid-record: should drop the fragment and parse the
	// remaining whole records.
	msgs, err := LoadTail(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatalf("expected some tail messages, got none")
	}
	for _, m := range msgs {
		if m.Kind != KindUser {
			t.Errorf("unexpected kind in tail: %v", m.Kind)
		}
	}
	t.Logf("LoadTail(1024) returned %d messages out of 50", len(msgs))
}

func TestLoadTailFullFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tail.jsonl")
	const data = `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	// Budget bigger than the file → no fragment skip, parse everything.
	msgs, err := LoadTail(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
}

func padding(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'x'
	}
	return string(out)
}
