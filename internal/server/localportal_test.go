package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/TakumaNakagame/ccdash/internal/settings"
	"github.com/TakumaNakagame/ccdash/internal/tunnel"
)

func linkClient(sess *yamux.Session) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return sess.Open() },
	}, Timeout: 5 * time.Second}
}

func linkDo(t *testing.T, c *http.Client, method, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://device"+path, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// checkLocalLink: the local portal gets the hub's allowlist (no token
// needed, hooks / shutdown / settings writes refused) and keeps working
// with hub_enabled off — that toggle is about the outbound hub tunnel.
func checkLocalLink(t *testing.T, s *Server, sess *yamux.Session) {
	t.Helper()
	c := linkClient(sess)
	if code, body := linkDo(t, c, "GET", "/api/sessions"); code != http.StatusOK {
		t.Fatalf("GET /api/sessions over the link = %d %s", code, body)
	}
	if code, body := linkDo(t, c, "GET", "/hub/info"); code != http.StatusOK || !strings.Contains(body, `"attachEnabled":true`) {
		t.Fatalf("GET /hub/info = %d %s", code, body)
	}
	for _, p := range [][2]string{{"PUT", "/api/settings/attach_enabled"}, {"POST", "/shutdown"}, {"POST", "/hooks/stop"}, {"GET", LocalPortalPath}} {
		if code, _ := linkDo(t, c, p[0], p[1]); code != http.StatusForbidden {
			t.Errorf("%s %s over the link = %d, want 403", p[0], p[1], code)
		}
	}
	ctx := context.Background()
	cur, _ := settings.Load(ctx, s.db)
	cur, err := settings.Set(ctx, s.db, cur, "hub_enabled", false)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := linkDo(t, c, "GET", "/api/sessions"); code != http.StatusOK {
		t.Errorf("GET /api/sessions with hub_enabled=0 = %d, want 200", code)
	}
	// The device-side gates still apply.
	if _, err := settings.Set(ctx, s.db, cur, "attach_enabled", false); err != nil {
		t.Fatal(err)
	}
	if code, _ := linkDo(t, c, "POST", "/pty/start"); code != http.StatusForbidden {
		t.Errorf("POST /pty/start with attach off = %d, want 403", code)
	}
}

func TestLocalPortalPipe(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hubEnd, devEnd := net.Pipe()
	done := make(chan struct{})
	go func() { _ = s.ServeLocalPortal(ctx, devEnd); close(done) }()
	sess, err := yamux.Client(hubEnd, tunnel.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	checkLocalLink(t, s, sess)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeLocalPortal did not return when its context ended")
	}
}

func TestLocalPortalUpgrade(t *testing.T) {
	s, _, token := newTestServer(t)
	srv := httptest.NewServer(s.mux)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := DialLocalPortal(ctx, addr, "wrong")
	var le *LocalPortalError
	if !errors.As(err, &le) || le.Status != http.StatusUnauthorized {
		t.Fatalf("wrong token: %v, want a 401 LocalPortalError", err)
	}
	sess, err := DialLocalPortal(ctx, addr, token)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	checkLocalLink(t, s, sess)

	// A plain GET (no Upgrade) is refused rather than hijacked.
	req, _ := http.NewRequest("GET", srv.URL+LocalPortalPath, nil)
	req.Header.Set("X-Ccdash-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("GET without Upgrade = %d, want 426", resp.StatusCode)
	}
}
