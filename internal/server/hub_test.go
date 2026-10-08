package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/takumanakagame/ccmanage/internal/hubcfg"
	"github.com/takumanakagame/ccmanage/internal/settings"
	"github.com/takumanakagame/ccmanage/internal/tunnel"
)

func TestHubAllowed(t *testing.T) {
	on := settings.Defaults()
	off := on
	off.AttachEnabled = false
	off.ApproveEnabled = false
	cases := []struct {
		method, path string
		cfg          settings.Settings
		want         bool
	}{
		{"GET", "/api/sessions", on, true},
		{"GET", "/api/approvals", on, true},
		{"GET", "/api/sessions/abc/transcript", on, true},
		{"GET", "/api/sessions/abc/usage", on, true},
		{"GET", "/api/usage", on, true},
		{"GET", "/api/usage/sessions", on, true},
		{"POST", "/api/sessions/abc/title", on, true},
		{"POST", "/api/sessions/abc/summarize", on, true},
		{"POST", "/api/titles", on, true},
		{"POST", "/api/sessions/abc/seen", on, true},
		{"POST", "/api/sessions/abc/delete", on, false},
		{"GET", "/api/settings", on, false},
		{"PUT", "/api/settings/approve_enabled", on, false},
		{"POST", "/hooks/stop", on, false},
		{"POST", "/shutdown", on, false},
		{"GET", "/pty/", off, true},
		{"POST", "/pty/start", on, true},
		{"POST", "/pty/start", off, false},
		{"GET", "/pty/abc/stream", off, false},
		{"POST", "/approvals/12/decide", on, true},
		{"POST", "/approvals/12/decide", off, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got, _ := hubAllowed(r, c.cfg); got != c.want {
			t.Errorf("%s %s (attach=%v) = %v, want %v", c.method, c.path, c.cfg.AttachEnabled, got, c.want)
		}
	}
}

// TestHubTunnelEndToEnd dials a fake hub, then drives the device through the
// tunnel the way the real hub does: allowed routes reach the mux without a
// token, blocked ones are refused, and flipping hub_enabled off cuts access.
func TestHubTunnelEndToEnd(t *testing.T) {
	s, d, _ := newTestServer(t)

	sessCh := make(chan *yamux.Session, 1)
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tunnel.Path || r.Header.Get("Authorization") != "Bearer ccdh_test" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		sess, err := tunnel.Accept(w, r)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		sessCh <- sess
		<-sess.CloseChan()
	}))
	defer hubSrv.Close()

	cfg := hubcfg.Config{URL: hubSrv.URL, Token: "ccdh_test"}
	if err := hubcfg.Save(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.hubServeOnce(ctx, cfg) }()

	var sess *yamux.Session
	select {
	case sess = <-sessCh:
	case <-time.After(5 * time.Second):
		t.Fatal("device never dialed the hub")
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return sess.Open() },
	}, Timeout: 5 * time.Second}
	get := func(method, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://device"+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := get("GET", "/api/sessions"); code != http.StatusOK {
		t.Fatalf("GET /api/sessions through tunnel = %d %s", code, body)
	}
	if code, body := get("GET", "/hub/settings"); code != http.StatusOK || !strings.Contains(body, `"key":"approve_enabled"`) {
		t.Fatalf("GET /hub/settings = %d %s", code, body)
	}
	if code, _ := get("GET", "/hub/skills"); code != http.StatusOK {
		t.Fatalf("GET /hub/skills = %d", code)
	}
	if code, body := get("GET", "/hub/info"); code != http.StatusOK || !strings.Contains(body, `"attachEnabled":true`) {
		t.Fatalf("GET /hub/info = %d %s", code, body)
	}
	for _, p := range [][2]string{{"PUT", "/api/settings/hub_enabled"}, {"POST", "/shutdown"}, {"POST", "/hooks/stop"}} {
		if code, _ := get(p[0], p[1]); code != http.StatusForbidden {
			t.Errorf("%s %s through tunnel = %d, want 403", p[0], p[1], code)
		}
	}

	// Image upload: stored 0600 under the state dir; non-images refused.
	upload := func(ct, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://device/hub/upload", strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body := upload("image/png", "\x89PNG fake")
	var up struct{ Path string }
	_ = json.Unmarshal([]byte(body), &up)
	if code != http.StatusOK || up.Path == "" {
		t.Fatalf("upload = %d %s", code, body)
	}
	if fi, err := os.Stat(up.Path); err != nil || fi.Mode().Perm() != 0o600 || !strings.HasSuffix(up.Path, ".png") {
		t.Errorf("uploaded file %s: %v %v", up.Path, fi, err)
	}
	if code, _ := upload("text/plain", "hi"); code != http.StatusUnsupportedMediaType {
		t.Errorf("text upload = %d, want 415", code)
	}

	if _, err := settings.Set(ctx, d, settings.Defaults(), "hub_enabled", false); err != nil {
		t.Fatal(err)
	}
	if code, _ := get("GET", "/api/sessions"); code != http.StatusForbidden {
		t.Errorf("GET /api/sessions with hub_enabled=0 = %d, want 403", code)
	}
}
