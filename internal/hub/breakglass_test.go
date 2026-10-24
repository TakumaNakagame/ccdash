package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newBreakGlassHub(t *testing.T, password string) *Hub {
	t.Helper()
	h, err := New(context.Background(), Config{
		DataDir:   t.TempDir(),
		PublicURL: "https://hub.example",
		Auth: AuthConfig{Issuer: "http://127.0.0.1:1", ClientID: "id", ClientSecret: "secret",
			AllowedEmails: []string{"me@example.com"}, BreakGlassPassword: password},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.auth.bgDelay = 0
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func breakGlassPost(handler http.Handler, password string) *httptest.ResponseRecorder {
	form := url.Values{"password": {password}}.Encode()
	return serve(handler, "POST", breakGlassPath, form, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

func sessionOf(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestBreakGlass(t *testing.T) {
	h := newBreakGlassHub(t, "open sesame")
	handler := h.Handler()

	if w := serve(handler, "GET", breakGlassPath, "", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `type="password"`) {
		t.Fatalf("form = %d %s", w.Code, w.Body)
	}
	w := breakGlassPost(handler, "wrong")
	if w.Code != http.StatusForbidden || sessionOf(w) != nil {
		t.Fatalf("wrong password = %d, cookie %v", w.Code, sessionOf(w))
	}
	w = breakGlassPost(handler, "open sesame")
	sess := sessionOf(w)
	if w.Code != http.StatusFound || sess == nil || !sess.HttpOnly || !sess.Secure {
		t.Fatalf("right password = %d, cookie %+v", w.Code, sess)
	}
	if sess.MaxAge > int(breakGlassTTL.Seconds()) {
		t.Errorf("cookie MaxAge = %d, want ≤ 1 h", sess.MaxAge)
	}
	r := httptest.NewRequest("GET", "/api/me", nil)
	r.AddCookie(sess)
	mw := httptest.NewRecorder()
	handler.ServeHTTP(mw, r)
	if mw.Code != http.StatusOK || !strings.Contains(mw.Body.String(), breakGlassUser) {
		t.Fatalf("/api/me with break-glass session = %d %s", mw.Code, mw.Body)
	}

	// A cross-site form post is refused before the password is checked.
	cw := serve(handler, "POST", breakGlassPath, "password=open+sesame",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "cross-site"})
	if cw.Code != http.StatusForbidden || sessionOf(cw) != nil {
		t.Errorf("cross-site post = %d", cw.Code)
	}

	// Without the password configured the session is dead and the door gone.
	off := newBreakGlassHub(t, "")
	off.auth.key = h.auth.key
	r = httptest.NewRequest("GET", "/api/me", nil)
	r.AddCookie(sess)
	if _, ok := off.auth.user(r); ok {
		t.Error("break-glass session accepted with the password removed")
	}
	if w := serve(off.Handler(), "GET", breakGlassPath, "", nil); w.Code != http.StatusFound {
		t.Errorf("door without password = %d, want the login redirect", w.Code)
	}
}

func TestBreakGlassLockout(t *testing.T) {
	h := newBreakGlassHub(t, "open sesame")
	handler := h.Handler()
	for range breakGlassMax {
		if w := breakGlassPost(handler, "guess"); w.Code != http.StatusForbidden {
			t.Fatalf("guess = %d", w.Code)
		}
	}
	w := breakGlassPost(handler, "open sesame")
	if w.Code != http.StatusForbidden || sessionOf(w) != nil || !strings.Contains(w.Body.String(), "locked") {
		t.Fatalf("right password while locked = %d %s", w.Code, w.Body)
	}
	// The failures age out of the window.
	h.auth.bgMu.Lock()
	for i := range h.auth.bgFails {
		h.auth.bgFails[i] = time.Now().Add(-breakGlassWin - time.Second)
	}
	h.auth.bgMu.Unlock()
	if w := breakGlassPost(handler, "open sesame"); w.Code != http.StatusFound {
		t.Fatalf("after the window = %d", w.Code)
	}
}

func TestBreakGlassLinkOnLoginFailure(t *testing.T) {
	// The issuer is unreachable: the login page offers the door.
	h := newBreakGlassHub(t, "open sesame")
	w := serve(h.Handler(), "GET", "/auth/login", "", nil)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), breakGlassPath) {
		t.Fatalf("login with provider down = %d %s", w.Code, w.Body)
	}
	off := newBreakGlassHub(t, "")
	w = serve(off.Handler(), "GET", "/auth/login", "", nil)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), breakGlassPath) {
		t.Fatalf("login without break glass = %d %s", w.Code, w.Body)
	}
}
