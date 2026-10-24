package hookcfg

import (
	"os"
	"strings"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/paths"
)

// TestNeedsResyncOldScript: a hook.sh from before the current revision
// (no CLAUDE_CODE_ENTRYPOINT header) is rewritten on the next resync.
func TestNeedsResyncOldScript(t *testing.T) {
	in := newInstall(t, "http://127.0.0.1:9123")
	if _, err := in.Apply(); err != nil {
		t.Fatal(err)
	}
	if stale, _ := NeedsResync(in.Path); stale {
		t.Fatal("fresh install: NeedsResync should be false")
	}
	script, _ := paths.HookScriptPath()
	b, _ := os.ReadFile(script)
	if !strings.Contains(string(b), "X-Ccdash-Entrypoint: ${CLAUDE_CODE_ENTRYPOINT:-}") {
		t.Fatal("hook.sh does not forward CLAUDE_CODE_ENTRYPOINT")
	}
	old := strings.Replace(string(b), hookScriptRev+"\n", "", 1)
	if err := os.WriteFile(script, []byte(old), 0o700); err != nil {
		t.Fatal(err)
	}
	if stale, _ := NeedsResync(in.Path); !stale {
		t.Fatal("old hook.sh: NeedsResync should be true")
	}
	if _, err := in.Apply(); err != nil {
		t.Fatal(err)
	}
	if stale, _ := NeedsResync(in.Path); stale {
		t.Fatal("after Apply: NeedsResync should be false")
	}
}
