package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newTestHub(t *testing.T, noAuth bool) *Hub {
	t.Helper()
	h, err := New(context.Background(), Config{
		Listen:    "127.0.0.1:0",
		DataDir:   t.TempDir(),
		PublicURL: "https://hub.example",
		Auth: AuthConfig{
			Issuer:        "https://issuer.example", // never contacted: no login flow runs here
			ClientID:      "id",
			ClientSecret:  "secret",
			AllowedEmails: []string{"Me@Example.com"},
			NoAuth:        noAuth,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func serve(h http.Handler, method, path string, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	// The test hubs' public URL; --no-auth / --tailscale-auth hubs refuse
	// other hosts (hostGuard).
	r.Host = "hub.example"
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestNewRefusesOpenHub(t *testing.T) {
	_, err := New(context.Background(), Config{DataDir: t.TempDir(), PublicURL: "https://x", Auth: AuthConfig{Issuer: "i", ClientID: "c", ClientSecret: "s"}})
	if err == nil {
		t.Fatal("expected an error without allowed emails")
	}
}

func TestSignedCookie(t *testing.T) {
	h := newTestHub(t, false)
	v := h.auth.sign(sessionClaims{Email: "me@example.com", Exp: time.Now().Add(time.Hour).Unix()})
	var sc sessionClaims
	if !h.auth.open(v, &sc) || sc.Email != "me@example.com" {
		t.Fatalf("round trip failed: %+v", sc)
	}
	body, mac, _ := strings.Cut(v, ".")
	if h.auth.open(body+"x."+mac, &sc) {
		t.Fatal("tampered cookie accepted")
	}

	// user(): valid, expired, and not-allowed sessions.
	check := func(c sessionClaims) bool {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: h.auth.sign(c)})
		_, ok := h.auth.user(r)
		return ok
	}
	if !check(sessionClaims{Email: "me@example.com", Exp: time.Now().Add(time.Hour).Unix()}) {
		t.Error("valid session refused")
	}
	if check(sessionClaims{Email: "me@example.com", Exp: time.Now().Add(-time.Minute).Unix()}) {
		t.Error("expired session accepted")
	}
	if check(sessionClaims{Email: "other@example.com", Exp: time.Now().Add(time.Hour).Unix()}) {
		t.Error("session for a removed email accepted")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/":                "/",
		"/#/d/x":           "/#/d/x",
		"//evil.example/":  "/",
		"https://evil/":    "/",
		"/\\evil.example/": "/",
		"":                 "/",
		"/\t/evil.example": "/",
		"/\n/evil.example": "/",
		"/a\\b":            "/",
		"/s/abc?x=1":       "/s/abc?x=1",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginRequired(t *testing.T) {
	h := newTestHub(t, false).Handler()
	if w := serve(h, "GET", "/api/devices", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("API without session = %d, want 401", w.Code)
	}
	w := serve(h, "GET", "/", "", nil)
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "/auth/login") {
		t.Errorf("page without session = %d %q, want redirect to login", w.Code, w.Header().Get("Location"))
	}
	if w := serve(h, "GET", "/healthz", "", nil); w.Code != http.StatusOK {
		t.Errorf("healthz = %d", w.Code)
	}
}

func TestDeviceLifecycle(t *testing.T) {
	hub := newTestHub(t, true)
	h := hub.Handler()
	json1 := map[string]string{"Content-Type": "application/json"}

	w := serve(h, "POST", "/api/devices", `{"name":"desk"}`, json1)
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var created struct {
		Device Device `json:"device"`
		Token  string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if !strings.HasPrefix(created.Token, "ccdh_") || created.Device.ID == "" {
		t.Fatalf("bad create response: %s", w.Body)
	}
	if w := serve(h, "POST", "/api/devices", `{"name":"desk"}`, json1); w.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", w.Code)
	}
	if _, err := hub.st.deviceByToken(context.Background(), created.Token); err != nil {
		t.Fatalf("token lookup: %v", err)
	}

	// A cross-site POST is refused even with a valid session.
	if w := serve(h, "POST", "/api/devices", `{"name":"x"}`, map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}); w.Code != http.StatusForbidden {
		t.Errorf("cross-site create = %d, want 403", w.Code)
	}

	// Rotation invalidates the old token.
	w = serve(h, "POST", "/api/devices/"+created.Device.ID+"/rotate", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate = %d", w.Code)
	}
	if _, err := hub.st.deviceByToken(context.Background(), created.Token); err == nil {
		t.Error("old token still valid after rotate")
	}

	// Unknown tokens can't open a tunnel.
	if w := serve(h, "GET", "/agent/connect", "", map[string]string{"Authorization": "Bearer " + created.Token}); w.Code != http.StatusUnauthorized {
		t.Errorf("tunnel with rotated token = %d, want 401", w.Code)
	}

	// Offline device: proxy answers 503; non-portal paths 404 regardless.
	if w := serve(h, "GET", "/api/d/"+created.Device.ID+"/api/sessions", "", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("offline proxy = %d, want 503", w.Code)
	}

	if w := serve(h, "DELETE", "/api/devices/"+created.Device.ID, "", nil); w.Code != http.StatusNoContent {
		t.Errorf("delete = %d", w.Code)
	}
	ds, _ := hub.st.listDevices(context.Background())
	if len(ds) != 0 {
		t.Errorf("devices after delete = %d", len(ds))
	}
}

func TestTailscaleServeAuth(t *testing.T) {
	h, err := New(context.Background(), Config{
		DataDir:   t.TempDir(),
		PublicURL: "https://box.example.ts.net",
		Auth:      AuthConfig{TailscaleServe: true, AllowedEmails: []string{"me@example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	handler := h.Handler()
	if w := serve(handler, "GET", "/api/me", "", map[string]string{"Host": "box.example.ts.net", "Tailscale-User-Login": "Me@Example.com"}); w.Code != http.StatusOK {
		t.Errorf("allowed login = %d", w.Code)
	}
	w := serve(handler, "GET", "/", "", map[string]string{"Host": "box.example.ts.net", "Tailscale-User-Login": "colleague@example.com"})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "colleague@example.com") {
		t.Errorf("other login = %d %s, want 403 naming the login", w.Code, w.Body)
	}
	if w := serve(handler, "GET", "/api/devices", "", map[string]string{"Host": "127.0.0.1:8080"}); w.Code != http.StatusForbidden {
		t.Errorf("no header = %d, want 403", w.Code)
	}
	// A rebinding page can set the login header itself; its Host gives it away.
	if w := serve(handler, "GET", "/api/me", "", map[string]string{"Host": "evil.example", "Tailscale-User-Login": "me@example.com"}); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("rebinding host = %d, want 421", w.Code)
	}
}

func TestPWAFilesArePublic(t *testing.T) {
	h := newTestHub(t, false).Handler()
	for _, p := range []string{"/manifest.webmanifest", "/sw.js", "/icon.svg", "/icon-192.png"} {
		if w := serve(h, "GET", p, "", nil); w.Code != http.StatusOK {
			t.Errorf("%s without login = %d, want 200", p, w.Code)
		}
	}
	if w := serve(h, "GET", "/manifest.webmanifest", "", nil); w.Header().Get("Content-Type") != "application/manifest+json" {
		t.Errorf("manifest content type = %q", w.Header().Get("Content-Type"))
	}
	if w := serve(h, "GET", "/app.js", "", nil); w.Code != http.StatusFound {
		t.Errorf("/app.js without login = %d, want redirect to login", w.Code)
	}
}

// TestPrefs: the prefs round-trip per user, a PUT merges, and bad values
// are refused.
func TestPrefs(t *testing.T) {
	h := newTestHub(t, true).Handler()
	if w := serve(h, "GET", "/api/prefs", "", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"theme":""`) {
		t.Fatalf("default = %d %s", w.Code, w.Body)
	}
	if w := serve(h, "PUT", "/api/prefs", `{"theme":"dark","accent":"#2F6BFF"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("put = %d %s", w.Code, w.Body)
	}
	if w := serve(h, "GET", "/api/prefs", "", nil); !strings.Contains(w.Body.String(), `"accent":"#2f6bff"`) || !strings.Contains(w.Body.String(), `"theme":"dark"`) {
		t.Fatalf("get = %s", w.Body)
	}
	// A PUT merges: an older page sending only the look keeps new_first.
	if w := serve(h, "PUT", "/api/prefs", `{"new_first":"off"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("put new_first = %d %s", w.Code, w.Body)
	}
	if w := serve(h, "PUT", "/api/prefs", `{"theme":"light","accent":""}`, nil); w.Code != http.StatusOK {
		t.Fatalf("put look = %d %s", w.Code, w.Body)
	}
	if w := serve(h, "GET", "/api/prefs", "", nil); !strings.Contains(w.Body.String(), `"new_first":"off"`) || !strings.Contains(w.Body.String(), `"theme":"light"`) {
		t.Fatalf("merged = %s", w.Body)
	}
	for _, bad := range []string{`{"theme":"blue"}`, `{"accent":"red"}`, `{"accent":"#12345"}`, `{"new_first":"yes"}`} {
		if w := serve(h, "PUT", "/api/prefs", bad, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d", bad, w.Code)
		}
	}
}

// TestHostGuard: a DNS-rebinding page arrives with its own name in Host.
// Hubs whose login such a page could satisfy (--no-auth, --tailscale-auth)
// accept only loopback names and the public host.
func TestHostGuard(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"LOCALHOST", true},
		{"[::1]:8080", true},
		{"hub.example", true},
		{"hub.example:443", true},
		{"evil.example:8080", false},
		{"127.0.0.1.evil.example", false},
		{"", false},
	}
	for _, c := range cases {
		if got := allowedHost(c.host, "hub.example"); got != c.want {
			t.Errorf("allowedHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}

	h := newTestHub(t, true)
	if w := serve(h.Handler(), "GET", "/api/me", "", map[string]string{"Host": "evil.example:8080"}); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("no-auth hub, rebinding host = %d, want 421", w.Code)
	}
	if w := serve(h.Handler(), "GET", "/api/me", "", map[string]string{"Host": "127.0.0.1:8080"}); w.Code != http.StatusOK {
		t.Errorf("no-auth hub, loopback host = %d, want 200", w.Code)
	}
	// An OIDC hub sits behind its proxy, which decides the Host; a
	// rebinding page has no session cookie for it anyway.
	oidc := newTestHub(t, false)
	if w := serve(oidc.Handler(), "GET", "/healthz", "", map[string]string{"Host": "anything.example"}); w.Code != http.StatusOK {
		t.Errorf("oidc hub healthz = %d, want 200", w.Code)
	}
}

// TestVendoredAssets: the portal loads nothing from a CDN (ADR 0005), and
// every vendor/ file the page or app.js names is embedded.
func TestVendoredAssets(t *testing.T) {
	var refs []string
	for _, f := range []string{"web/index.html", "web/app.js", "web/app.css"} {
		b, err := webFS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, cdn := range []string{"cdn.jsdelivr.net", "unpkg.com", "fonts.googleapis.com", "fonts.gstatic.com"} {
			if strings.Contains(s, cdn) {
				t.Errorf("%s still references %s", f, cdn)
			}
		}
		for _, m := range vendorRef.FindAllString(s, -1) {
			refs = append(refs, m)
		}
	}
	if len(refs) < 6 {
		t.Fatalf("found only %d vendor references: %v", len(refs), refs)
	}
	h := newTestHub(t, true).Handler()
	for _, ref := range refs {
		if _, err := webFS.ReadFile("web/" + ref); err != nil {
			t.Errorf("%s is referenced but not embedded", ref)
			continue
		}
		w := serve(h, "GET", "/"+ref, "", nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("GET /%s = %d, Cache-Control %q", ref, w.Code, w.Header().Get("Cache-Control"))
		}
	}
	// Behind login like the page itself.
	if w := serve(newTestHub(t, false).Handler(), "GET", "/"+refs[0], "", nil); w.Code != http.StatusFound {
		t.Errorf("vendor file without login = %d, want a redirect to login", w.Code)
	}
}

var vendorRef = regexp.MustCompile(`vendor/[A-Za-z0-9._@-]+/[A-Za-z0-9._-]+\.(?:js|css)`)
