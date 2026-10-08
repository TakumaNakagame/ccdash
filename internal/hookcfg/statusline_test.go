package hookcfg

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStatusLineRelay: install wraps the operator's statusLine (keeping its
// other keys), the relay forwards the JSON and still prints the original's
// output, and uninstall puts the original back exactly.
func TestStatusLineRelay(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path == StatusLineEndpoint && r.Header.Get("X-Ccdash-Token") != "" {
			got <- string(b)
		}
	}))
	defer srv.Close()
	if err := os.MkdirAll(filepath.Join(dir, "ccdash"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "ccdash", "token"), []byte("tok123\n"), 0o600)

	settings := filepath.Join(dir, "settings.json")
	orig := `{"statusLine":{"type":"command","command":"cat >/dev/null; echo mine","padding":2},"theme":"dark"}`
	_ = os.WriteFile(settings, []byte(orig), 0o600)

	in := &Install{BaseURL: srv.URL, Path: settings}
	if err := in.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	if err := in.InstallStatusLine(); err != nil { // idempotent: keeps the true original
		t.Fatal(err)
	}
	m, _ := readSettings(settings)
	sl := m["statusLine"].(map[string]any)
	if !isManagedStatusLine(sl) || sl["padding"] != float64(2) {
		t.Fatalf("statusLine = %v", sl)
	}

	script := filepath.Join(dir, "ccdash", "statusline.sh")
	cmd := exec.Command("sh", "-c", sl["command"].(string))
	cmd.Stdin = strings.NewReader(`{"session_id":"s1"}`)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run %s: %v", script, err)
	}
	if strings.TrimSpace(string(out)) != "mine" {
		t.Errorf("output = %q, want the original's", out)
	}
	select {
	case b := <-got:
		if b != `{"session_id":"s1"}` {
			t.Errorf("relayed %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("status JSON never reached the collector")
	}

	if err := in.RemoveStatusLine(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(settings)
	var a, want any
	_ = json.Unmarshal(b, &a)
	_ = json.Unmarshal([]byte(orig), &want)
	if ab, wb := mustJSON(a), mustJSON(want); ab != wb {
		t.Errorf("restored %s, want %s", ab, wb)
	}
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Error("relay script left behind")
	}
}

// TestStatusLineNoOriginal: with no statusLine of its own the relay prints
// nothing, and uninstall removes the key again.
func TestStatusLineNoOriginal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	settings := filepath.Join(dir, "settings.json")
	_ = os.WriteFile(settings, []byte(`{}`), 0o600)
	in := &Install{BaseURL: "http://127.0.0.1:1", Path: settings}
	if err := in.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	m, _ := readSettings(settings)
	cmd := exec.Command("sh", "-c", m["statusLine"].(map[string]any)["command"].(string))
	cmd.Stdin = strings.NewReader(`{}`)
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if err := in.RemoveStatusLine(); err != nil {
		t.Fatal(err)
	}
	m, _ = readSettings(settings)
	if _, ok := m["statusLine"]; ok {
		t.Errorf("statusLine left: %v", m["statusLine"])
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
