package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFromTranscript: the skill_listing attachments name the session's
// skills (initial list + deltas), read again only past the last offset.
func TestFromTranscript(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	initial := `{"type":"attachment","attachment":{"type":"skill_listing","content":"- simplify: Review the changed code\n- anthropic-skills:pdf: PDF files","names":["simplify","anthropic-skills:pdf"],"isInitial":true}}` + "\n"
	if err := os.WriteFile(p, []byte(`{"type":"user","message":{"content":"skill_listing mentioned"}}`+"\n"+initial), 0o644); err != nil {
		t.Fatal(err)
	}
	got := FromTranscript(p)
	if len(got) != 2 || got[0].Name != "simplify" || got[0].Description != "Review the changed code" || got[1].Name != "anthropic-skills:pdf" || got[1].Source != "session" {
		t.Fatalf("initial: %+v", got)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"type":"attachment","attachment":{"type":"skill_listing","content":"- publish-artifact: Put a page on artifacts","names":["publish-artifact","simplify"],"isInitial":false}}` + "\n")
	_ = f.Close()
	got = FromTranscript(p)
	if len(got) != 3 || got[2].Name != "publish-artifact" || got[2].Description != "Put a page on artifacts" {
		t.Fatalf("delta: %+v", got)
	}
	if FromTranscript(filepath.Join(t.TempDir(), "none.jsonl")) != nil || FromTranscript("") != nil {
		t.Fatal("missing file should give nil")
	}
}
