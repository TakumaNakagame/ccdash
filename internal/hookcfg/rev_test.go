package hookcfg

import (
	"fmt"
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

// TestForwardsIgnore: both the forwarder and the blocking PermissionRequest
// hook pass $CCDASH_IGNORE on, and a PermissionRequest entry from before
// that is rewritten on the next resync.
func TestForwardsIgnore(t *testing.T) {
	in := newInstall(t, "http://127.0.0.1:9123")
	if _, err := in.Apply(); err != nil {
		t.Fatal(err)
	}
	script, _ := paths.HookScriptPath()
	b, _ := os.ReadFile(script)
	if !strings.Contains(string(b), "X-Ccdash-Ignore: ${CCDASH_IGNORE:-}") {
		t.Fatal("hook.sh does not forward CCDASH_IGNORE")
	}
	settings, err := readSettings(in.Path)
	if err != nil {
		t.Fatal(err)
	}
	hooks := settings["hooks"].(map[string]any)
	entry := hooks["PermissionRequest"].([]any)[0].(map[string]any)
	h := entry["hooks"].([]any)[0].(map[string]any)
	hdr := h["headers"].(map[string]any)
	if hdr[IgnoreHeaderKey] != "${CCDASH_IGNORE}" {
		t.Fatalf("PermissionRequest headers: %v", hdr)
	}
	if !strings.Contains(fmt.Sprint(h["allowedEnvVars"]), "CCDASH_IGNORE") {
		t.Fatalf("allowedEnvVars: %v", h["allowedEnvVars"])
	}
	delete(hdr, IgnoreHeaderKey)
	if err := writeSettings(in.Path, settings); err != nil {
		t.Fatal(err)
	}
	if stale, _ := NeedsResync(in.Path); !stale {
		t.Fatal("PermissionRequest without X-Ccdash-Ignore: NeedsResync should be true")
	}
}
