package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadRealTranscripts runs the parser over every transcript file in
// ~/.claude/projects (if present) to make sure malformed lines don't crash
// the loader. This is local-machine smoke coverage; it skips entirely when
// the directory isn't there.
func TestLoadRealTranscripts(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	base := filepath.Join(home, ".claude", "projects")
	if _, err := os.Stat(base); err != nil {
		t.Skipf("no transcripts at %s", base)
	}
	count := 0
	err = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		count++
		if count > 5 {
			return filepath.SkipDir
		}
		msgs, err := Load(path)
		if err != nil {
			t.Errorf("load %s: %v", path, err)
			return nil
		}
		t.Logf("%s → %d messages", filepath.Base(path), len(msgs))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
