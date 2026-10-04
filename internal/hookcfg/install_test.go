package hookcfg

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/paths"
)

// newInstall points the state dir at a temp dir and returns an Install that
// writes a temp settings.json.
func newInstall(t *testing.T, baseURL string) *Install {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	return &Install{BaseURL: baseURL, Path: filepath.Join(dir, "settings.json")}
}

func loadHooks(t *testing.T, path string) map[string]any {
	t.Helper()
	s, err := readSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	hooks, _ := s["hooks"].(map[string]any)
	return hooks
}

// handlerOf returns the single handler of the only managed entry for event.
func handlerOf(t *testing.T, hooks map[string]any, event string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, item := range hooks[event].([]any) {
		if isManagedEntry(item) {
			hs := item.(map[string]any)["hooks"].([]any)
			found = append(found, hs[0].(map[string]any))
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: want 1 managed entry, got %d", event, len(found))
	}
	return found[0]
}

func TestApplyLayout(t *testing.T) {
	in := newInstall(t, "http://127.0.0.1:9123")
	if _, err := in.Apply(); err != nil {
		t.Fatal(err)
	}
	hooks := loadHooks(t, in.Path)
	script, _ := paths.HookScriptPath()
	for event, endpoint := range Endpoints {
		h := handlerOf(t, hooks, event)
		if event == "PermissionRequest" {
			if h["type"] != "http" || h["url"] != "http://127.0.0.1:9123"+endpoint {
				t.Errorf("PermissionRequest: want blocking http hook, got %v", h)
			}
			continue
		}
		if h["type"] != "command" || h["command"] != shellQuote(script)+" "+endpoint {
			t.Errorf("%s: want forwarder command, got %v", event, h)
		}
	}
	fi, err := os.Stat(script)
	if err != nil {
		t.Fatalf("hook script not written: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("hook script perm = %v, want 0700", fi.Mode().Perm())
	}
}

func TestApplyIdempotentAndKeepsForeignHooks(t *testing.T) {
	in := newInstall(t, "http://127.0.0.1:9123")
	foreign := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo mine"}}}
	if err := writeSettings(in.Path, map[string]any{
		"hooks": map[string]any{"Stop": []any{foreign}},
		"theme": "dark",
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := in.Apply(); err != nil {
			t.Fatal(err)
		}
	}
	stop := loadHooks(t, in.Path)["Stop"].([]any)
	if len(stop) != 2 {
		t.Fatalf("Stop: want foreign + 1 managed entry, got %d: %v", len(stop), stop)
	}
	if isManagedEntry(stop[0]) {
		t.Errorf("foreign entry was replaced: %v", stop[0])
	}
	s, _ := readSettings(in.Path)
	if s["theme"] != "dark" {
		t.Errorf("unrelated setting lost: %v", s["theme"])
	}
}

func TestApplyReplacesLegacyHTTPEntries(t *testing.T) {
	in := newInstall(t, "http://127.0.0.1:9123")
	legacy := buildHTTPEntry("http://127.0.0.1:9123/hooks/stop", "old", 30)
	if err := writeSettings(in.Path, map[string]any{
		"hooks": map[string]any{"Stop": []any{legacy}},
	}); err != nil {
		t.Fatal(err)
	}
	stale, err := NeedsResync(in.Path)
	if err != nil || !stale {
		t.Fatalf("legacy http Stop entry: NeedsResync = %v, %v; want true", stale, err)
	}
	if _, err := in.Apply(); err != nil {
		t.Fatal(err)
	}
	if h := handlerOf(t, loadHooks(t, in.Path), "Stop"); h["type"] != "command" {
		t.Errorf("legacy entry not migrated: %v", h)
	}
	if stale, _ := NeedsResync(in.Path); stale {
		t.Error("NeedsResync still true after Apply")
	}
	if tok, _ := InstalledTokenAt(in.Path); tok == "" || tok == "old" {
		t.Errorf("InstalledTokenAt = %q, want the current token from PermissionRequest", tok)
	}
}

func TestNeedsResyncMissingScript(t *testing.T) {
	in := newInstall(t, "http://127.0.0.1:9123")
	if stale, _ := NeedsResync(in.Path); stale {
		t.Error("nothing installed: NeedsResync should be false")
	}
	if _, err := in.Apply(); err != nil {
		t.Fatal(err)
	}
	script, _ := paths.HookScriptPath()
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	if stale, _ := NeedsResync(in.Path); !stale {
		t.Error("missing hook.sh: NeedsResync should be true")
	}
}

func TestRemove(t *testing.T) {
	in := newInstall(t, "http://127.0.0.1:9123")
	foreign := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo mine"}}}
	if err := writeSettings(in.Path, map[string]any{
		"hooks": map[string]any{"Stop": []any{foreign}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := in.Remove(); err != nil {
		t.Fatal(err)
	}
	hooks := loadHooks(t, in.Path)
	for event, raw := range hooks {
		for _, item := range raw.([]any) {
			if isManagedEntry(item) {
				t.Errorf("%s: managed entry survived Remove", event)
			}
		}
	}
	if stop, _ := hooks["Stop"].([]any); len(stop) != 1 {
		t.Errorf("foreign Stop entry lost: %v", hooks["Stop"])
	}
	script, _ := paths.HookScriptPath()
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Errorf("hook script not removed: %v", err)
	}
}

// TestForwarderPosts runs the generated script end to end: it must return
// at once and deliver the payload with the token from the token file.
func TestForwarderPosts(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	type hit struct{ path, token, wrapper, body string }
	got := make(chan hit, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- hit{r.URL.Path, r.Header.Get(TokenHeaderKey), r.Header.Get("X-Ccdash-Wrapper-Pid"), string(b)}
	}))
	defer srv.Close()

	in := newInstall(t, srv.URL)
	if _, err := in.Apply(); err != nil {
		t.Fatal(err)
	}
	tokPath, _ := paths.TokenPath()
	tokBytes, err := os.ReadFile(tokPath)
	if err != nil {
		t.Fatal(err)
	}
	h := handlerOf(t, loadHooks(t, in.Path), "Stop")

	payload := `{"session_id":"abc"}`
	cmd := exec.Command("sh", "-c", h["command"].(string))
	cmd.Stdin = strings.NewReader(payload)
	cmd.Env = append(os.Environ(), "CCDASH_WRAPPER_PID=42")
	start := time.Now()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("forwarder failed: %v: %s", err, out)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("forwarder took %v; it must not wait for the POST", d)
	}

	select {
	case hh := <-got:
		if hh.path != "/hooks/stop" || hh.body != payload || hh.wrapper != "42" {
			t.Errorf("unexpected request: %+v", hh)
		}
		if hh.token != strings.TrimSpace(string(tokBytes)) {
			t.Errorf("token = %q, want the token file's", hh.token)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forwarder never POSTed")
	}
}

// TestForwarderDeadCollector: with nothing listening the hook still exits 0
// immediately, so a stopped ccdash never stalls or errors the session.
func TestForwarderDeadCollector(t *testing.T) {
	in := newInstall(t, "http://127.0.0.1:1")
	if _, err := in.Apply(); err != nil {
		t.Fatal(err)
	}
	h := handlerOf(t, loadHooks(t, in.Path), "UserPromptSubmit")
	cmd := exec.Command("sh", "-c", h["command"].(string))
	cmd.Stdin = strings.NewReader(`{}`)
	start := time.Now()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("forwarder failed: %v: %s", err, out)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("forwarder took %v against a dead collector", d)
	}
}
