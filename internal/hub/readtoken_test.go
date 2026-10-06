package hub

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testReadToken = "0123456789abcdef0123456789abcdef0123456789abcdef"

func newReadTokenHub(t *testing.T, noAuth bool) *Hub {
	t.Helper()
	h, err := New(context.Background(), Config{
		Listen:    "127.0.0.1:0",
		DataDir:   t.TempDir(),
		PublicURL: "https://hub.example",
		Auth: AuthConfig{
			Issuer:        "https://issuer.example",
			ClientID:      "id",
			ClientSecret:  "secret",
			AllowedEmails: []string{"me@example.com"},
			NoAuth:        noAuth,
		},
		ReadTokens: []string{testReadToken},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// proxiedDevice plugs a device whose collector is an httptest server into the
// hub, recording what reached it.
type proxiedDevice struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (f *proxiedDevice) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return nil
	}
	return f.reqs[len(f.reqs)-1]
}

func attachFakeDevice(t *testing.T, h *Hub, id string) *proxiedDevice {
	t.Helper()
	f := &proxiedDevice{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Clone(context.Background()))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	h.mu.Lock()
	h.conns[id] = &deviceConn{since: time.Now(), transport: tr, proxy: newDeviceProxy(tr)}
	h.mu.Unlock()
	return f
}

func bearer(tok string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tok}
}

func TestParseReadTokens(t *testing.T) {
	toks, err := ParseReadTokens(" " + testReadToken + " ,\n# comment\n\n" + strings.ToUpper(testReadToken))
	if err != nil || len(toks) != 2 || toks[0] != testReadToken {
		t.Fatalf("ParseReadTokens = %q, %v", toks, err)
	}
	if _, err := ParseReadTokens("short"); err == nil {
		t.Error("short token accepted")
	}
	if toks, err := ParseReadTokens(""); err != nil || len(toks) != 0 {
		t.Errorf("empty = %q, %v", toks, err)
	}
}

func TestReadTokenAllowedRoutes(t *testing.T) {
	h := newReadTokenHub(t, false)
	dev := attachFakeDevice(t, h, "dev1")
	handler := h.Handler()

	for _, p := range []string{"/api/board", "/api/active", "/api/devices"} {
		w := serve(handler, "GET", p, "", bearer(testReadToken))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s with read token = %d %s", p, w.Code, w.Body)
		}
	}
	w := serve(handler, "GET", "/api/board", "", bearer(testReadToken))
	var board map[string][]boardCard
	if err := json.Unmarshal(w.Body.Bytes(), &board); err != nil || board["needs_you"] == nil {
		t.Errorf("board shape: %s (%v)", w.Body, err)
	}

	// Device-proxied reads reach the device without the bearer credential.
	w = serve(handler, "GET", "/api/d/dev1/api/sessions", "", bearer(testReadToken))
	if w.Code != http.StatusOK {
		t.Fatalf("proxied sessions = %d %s", w.Code, w.Body)
	}
	got := dev.last()
	if got == nil || got.URL.Path != "/api/sessions" {
		t.Fatalf("device saw %v", got)
	}
	if got.Header.Get("Authorization") != "" || got.Header.Get("Cookie") != "" {
		t.Error("credentials forwarded to the device")
	}

	w = serve(handler, "GET", "/api/d/dev1/api/sessions/abc/transcript?mode=tail&lines=20&bytes=99999999", "", bearer(testReadToken))
	if w.Code != http.StatusOK {
		t.Fatalf("proxied transcript = %d %s", w.Code, w.Body)
	}
	got = dev.last()
	if got.URL.Path != "/api/sessions/abc/transcript" || got.URL.Query().Get("bytes") != "1048576" || got.URL.Query().Get("lines") != "20" {
		t.Errorf("transcript request reached device as %s", got.URL)
	}
	if w := serve(handler, "GET", "/api/d/dev1/api/sessions/abc/transcript?mode=full", "", bearer(testReadToken)); w.Code != http.StatusForbidden {
		t.Errorf("mode=full with read token = %d, want 403", w.Code)
	}
}

func TestReadTokenWrong(t *testing.T) {
	handler := newReadTokenHub(t, false).Handler()
	for _, tok := range []string{"nope", testReadToken + "x", ""} {
		w := serve(handler, "GET", "/api/board", "", bearer(tok))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("token %q = %d, want 401", tok, w.Code)
		}
		if !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("token %q: no WWW-Authenticate", tok)
		}
		if strings.Contains(w.Body.String(), testReadToken) {
			t.Error("response echoes a token")
		}
	}
}

func TestReadTokenForbiddenRoutes(t *testing.T) {
	h := newReadTokenHub(t, false)
	attachFakeDevice(t, h, "dev1")
	handler := h.Handler()
	cases := []struct{ method, path string }{
		{"POST", "/api/devices"},
		{"DELETE", "/api/devices/dev1"},
		{"POST", "/api/devices/dev1/rotate"},
		{"PUT", "/api/quick"},
		{"GET", "/api/quick"},
		{"GET", "/api/me"},
		{"GET", "/api/push/key"},
		{"POST", "/api/push/test"},
		{"POST", "/api/push/subscribe"},
		{"GET", "/api/d/dev1/ws/pty/k"},
		{"GET", "/api/d/dev1/pty"},
		{"POST", "/api/d/dev1/pty/start"},
		{"GET", "/api/d/dev1/api/approvals"},
		{"POST", "/api/d/dev1/approvals/1/decide"},
		{"POST", "/api/d/dev1/api/sessions/abc/seen"},
		{"GET", "/api/d/dev1/api/settings"},
		{"GET", "/api/d/dev1/hub/settings"},
		{"HEAD", "/api/board"},
	}
	for _, c := range cases {
		if w := serve(handler, c.method, c.path, "{}", bearer(testReadToken)); w.Code != http.StatusForbidden {
			t.Errorf("%s %s with read token = %d, want 403", c.method, c.path, w.Code)
		}
	}
}

func TestCookieFlowUnchangedWithReadTokens(t *testing.T) {
	h := newReadTokenHub(t, false)
	handler := h.Handler()
	// No credentials: still 401 / login redirect.
	if w := serve(handler, "GET", "/api/board", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("no session = %d, want 401", w.Code)
	}
	cookie := h.auth.sign(sessionClaims{Email: "me@example.com", Exp: time.Now().Add(time.Hour).Unix()})
	withCookie := func(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(`{"name":"desk"}`))
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := withCookie("GET", "/api/me", nil); w.Code != http.StatusOK {
		t.Errorf("cookie GET /api/me = %d", w.Code)
	}
	if w := withCookie("POST", "/api/devices", map[string]string{"Content-Type": "application/json"}); w.Code != http.StatusOK {
		t.Errorf("cookie POST = %d %s", w.Code, w.Body)
	}
	// Cross-origin protection still applies to cookie writes.
	if w := withCookie("POST", "/api/devices", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}); w.Code != http.StatusForbidden {
		t.Errorf("cross-site cookie POST = %d, want 403", w.Code)
	}
	// A bearer header does not upgrade a cookie request: write routes stay
	// refused for it.
	if w := withCookie("POST", "/api/devices", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + testReadToken}); w.Code != http.StatusForbidden {
		t.Errorf("cookie+bearer POST = %d, want 403", w.Code)
	}
}

func TestBearerIgnoredWithoutReadTokens(t *testing.T) {
	// With no read tokens configured, a bearer header changes nothing: the
	// request still needs a login.
	handler := newTestHub(t, false).Handler()
	if w := serve(handler, "GET", "/api/board", "", bearer(testReadToken)); w.Code != http.StatusUnauthorized {
		t.Errorf("bearer without configured tokens = %d, want 401 (login)", w.Code)
	}
}

func TestShortReadTokenRefused(t *testing.T) {
	_, err := New(context.Background(), Config{
		DataDir: t.TempDir(), PublicURL: "https://x",
		Auth:       AuthConfig{NoAuth: true},
		ReadTokens: []string{"short"},
	})
	if err == nil {
		t.Fatal("short read token accepted")
	}
}
