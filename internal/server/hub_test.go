package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		{"GET", "/api/sessions/abc/statusline", on, true},
		{"POST", "/hooks/statusline", on, false},
		{"GET", "/api/usage", on, true},
		{"GET", "/api/usage/sessions", on, true},
		{"POST", "/api/sessions/abc/title", on, true},
		{"POST", "/api/sessions/abc/summarize", on, false},
		{"POST", "/api/sessions/abc/project", on, true},
		{"POST", "/api/projects/color", on, true},
		{"GET", "/api/projects", on, true},
		{"POST", "/api/sessions/abc/later", on, true},
		{"POST", "/api/sessions/abc/grid", on, true},
		{"POST", "/api/projects/rename", on, true},
		{"POST", "/api/projects/order", on, true},
		{"POST", "/api/projects/x/color", on, false},
		{"GET", "/api/tasks", on, true},
		{"GET", "/api/tasks/11", on, true},
		{"GET", "/api/tasks/context", on, true},
		{"POST", "/api/tasks", on, true},
		{"POST", "/api/tasks/11", on, true},
		{"POST", "/api/tasks/11/notes", on, true},
		{"POST", "/api/tasks/order", on, true},
		{"POST", "/api/tasks/sync", on, false},
		{"POST", "/api/tasks/import", on, false},
		{"POST", "/api/tasks/11/other", on, false},
		{"DELETE", "/api/tasks/11", on, false},
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
		{"POST", "/pty/abc/restart", on, true},
		{"POST", "/pty/abc/restart", off, false},
		{"GET", "/api/restart", off, true},
		{"POST", "/api/restart", on, true},
		{"POST", "/api/restart", off, false},
		{"DELETE", "/api/restart", on, true},
		{"DELETE", "/api/restart", off, false},
		{"PUT", "/api/restart", on, false},
		{"POST", "/api/sessions/abc/color", on, true},
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

	// Upload: stored 0600 under the state dir, any type, the sent name kept
	// (sanitized) in the stored one.
	upload := func(ct, body string, name ...string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://device/hub/upload", strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		if len(name) > 0 {
			req.Header.Set("X-Filename", url.QueryEscape(name[0]))
		}
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
	code, body = upload("application/pdf", "%PDF-1.4", "見積 書 (v2).PDF")
	up.Path = ""
	_ = json.Unmarshal([]byte(body), &up)
	if code != http.StatusOK || !strings.HasSuffix(up.Path, "-見積_書__v2.pdf") {
		t.Errorf("pdf upload = %d %s", code, body)
	}

	if _, err := settings.Set(ctx, d, settings.Defaults(), "hub_enabled", false); err != nil {
		t.Fatal(err)
	}
	if code, _ := get("GET", "/api/sessions"); code != http.StatusForbidden {
		t.Errorf("GET /api/sessions with hub_enabled=0 = %d, want 403", code)
	}
}

func TestUploadName(t *testing.T) {
	for _, c := range []struct{ sent, ct, want string }{
		{"", "image/png", ".png"},
		{"", "application/octet-stream", ".bin"},
		{"report.pdf", "", "-report.pdf"},
		{"../../etc/passwd", "", "-passwd.bin"},
		{`C:\\Users\\me\\a b.txt`, "text/plain", "-a_b.txt"},
		{"$(rm -rf).sh", "", "-rm_-rf.sh"},
		{".bashrc", "", ".bashrc"},
	} {
		if got := uploadName(c.sent, c.ct); got != c.want {
			t.Errorf("uploadName(%q, %q) = %q, want %q", c.sent, c.ct, got, c.want)
		}
	}
}
