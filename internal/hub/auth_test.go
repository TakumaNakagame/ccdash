package hub

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// fakeIssuer is a minimal OIDC provider: discovery, JWKS, and a token
// endpoint that mints an id_token for whatever email the test chose.
type fakeIssuer struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	email string
	nonce string // captured from the authorize redirect
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, nil)
		claims, _ := json.Marshal(map[string]any{
			"iss": f.srv.URL, "aud": "id", "sub": "u1", "email": f.email, "nonce": f.nonce,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		})
		jws, _ := signer.Sign(claims)
		idt, _ := jws.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idt, "expires_in": 3600})
	})
	t.Cleanup(f.srv.Close)
	return f
}

func TestOIDCLogin(t *testing.T) {
	f := newFakeIssuer(t)
	h, err := New(context.Background(), Config{
		DataDir:   t.TempDir(),
		PublicURL: "https://hub.example",
		Auth:      AuthConfig{Issuer: f.srv.URL, ClientID: "id", ClientSecret: "secret", AllowedEmails: []string{"me@example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	handler := h.Handler()

	login := func(email string) *httptest.ResponseRecorder {
		f.email = email
		w := serve(handler, "GET", "/auth/login?next=/%23/d/x", "", nil)
		if w.Code != http.StatusFound {
			t.Fatalf("login = %d", w.Code)
		}
		loc, _ := url.Parse(w.Header().Get("Location"))
		if !strings.HasPrefix(loc.String(), f.srv.URL+"/authorize") || loc.Query().Get("code_challenge") == "" {
			t.Fatalf("authorize redirect = %s", loc)
		}
		if got := loc.Query().Get("redirect_uri"); got != "https://hub.example/auth/callback" {
			t.Fatalf("redirect_uri = %s", got)
		}
		f.nonce = loc.Query().Get("nonce")
		r := httptest.NewRequest("GET", "/auth/callback?code=good-code&state="+loc.Query().Get("state"), nil)
		for _, c := range w.Result().Cookies() {
			r.AddCookie(c)
		}
		cw := httptest.NewRecorder()
		handler.ServeHTTP(cw, r)
		return cw
	}

	cw := login("Me@Example.com")
	if cw.Code != http.StatusFound || cw.Header().Get("Location") != "/#/d/x" {
		t.Fatalf("callback = %d %q %s", cw.Code, cw.Header().Get("Location"), cw.Body)
	}
	var sess *http.Cookie
	for _, c := range cw.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			sess = c
		}
	}
	if sess == nil || !sess.HttpOnly || !sess.Secure {
		t.Fatalf("session cookie missing or not HttpOnly+Secure: %+v", sess)
	}
	r := httptest.NewRequest("GET", "/api/me", nil)
	r.AddCookie(sess)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "me@example.com") {
		t.Fatalf("/api/me = %d %s", w.Code, w.Body)
	}

	if cw := login("stranger@example.com"); cw.Code != http.StatusForbidden {
		t.Errorf("login outside the allowlist = %d, want 403", cw.Code)
	}

	// Two tabs start a login at once (an expired session): each callback
	// finds its own state cookie, so neither fails with a state mismatch.
	f.email = "me@example.com"
	var starts []*httptest.ResponseRecorder
	for range 2 {
		starts = append(starts, serve(handler, "GET", "/auth/login?next=/", "", nil))
	}
	jar := map[string]*http.Cookie{}
	for _, w := range starts {
		for _, c := range w.Result().Cookies() {
			jar[c.Name] = c
		}
	}
	for i, w := range starts {
		loc, _ := url.Parse(w.Header().Get("Location"))
		f.nonce = loc.Query().Get("nonce")
		r := httptest.NewRequest("GET", "/auth/callback?code=good-code&state="+loc.Query().Get("state"), nil)
		for _, c := range jar {
			r.AddCookie(c)
		}
		cw := httptest.NewRecorder()
		handler.ServeHTTP(cw, r)
		if cw.Code != http.StatusFound {
			t.Fatalf("concurrent login %d callback = %d %s", i, cw.Code, cw.Body)
		}
	}

	// A late callback whose login cookie is gone but whose browser is
	// already logged in goes back to the portal instead of erroring.
	r = httptest.NewRequest("GET", "/auth/callback?code=good-code&state=x", nil)
	r.AddCookie(sess)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Errorf("stale callback while logged in = %d, want 302", w.Code)
	}

	// A callback without the login cookie (a login page left open past
	// its lifetime, or a replay) starts a fresh login once instead of an
	// error; the code is never exchanged.
	w = serve(handler, "GET", "/auth/callback?code=good-code&state=x", "", nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/auth/login?next=/" {
		t.Fatalf("stale callback = %d %q, want a new login", w.Code, w.Header().Get("Location"))
	}
	var relogin *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == reloginCookie {
			relogin = c
		}
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("stale callback issued a session")
		}
	}
	if relogin == nil {
		t.Fatal("no relogin marker")
	}
	// The second one within reloginTTL is an error, so it can't loop.
	r = httptest.NewRequest("GET", "/auth/callback?code=good-code&state=x", nil)
	r.AddCookie(relogin)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("repeated stale callback = %d, want 400", w.Code)
	}
	// A provider error is shown, not retried.
	if w := serve(handler, "GET", "/auth/callback?error=access_denied&state=x", "", nil); w.Code != http.StatusBadRequest {
		t.Errorf("stale callback with error = %d, want 400", w.Code)
	}
}

func TestSessionRenewAndLogoutOthers(t *testing.T) {
	h, err := New(context.Background(), Config{
		DataDir:   t.TempDir(),
		PublicURL: "https://hub.example",
		Auth: AuthConfig{Issuer: "https://issuer.example", ClientID: "id", ClientSecret: "s",
			AllowedEmails: []string{"me@example.com"}, SessionTTL: 30 * 24 * time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	handler := h.Handler()
	mk := func(c sessionClaims) *http.Cookie {
		return &http.Cookie{Name: sessionCookie, Value: h.auth.sign(c)}
	}
	get := func(c *http.Cookie, method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Host = "hub.example"
		r.AddCookie(c)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	now := time.Now()

	// Fresh: /api/me leaves it alone.
	fresh := mk(sessionClaims{Email: "me@example.com", Exp: now.Add(29 * 24 * time.Hour).Unix(), Iat: now.Unix()})
	if w := get(fresh, "GET", "/api/me"); w.Code != 200 || sessionOf(w) != nil {
		t.Fatalf("fresh /api/me = %d, renewed=%v", w.Code, sessionOf(w) != nil)
	}
	// Past half: slid forward to the full TTL, same Iat.
	old := mk(sessionClaims{Email: "me@example.com", Exp: now.Add(2 * 24 * time.Hour).Unix(), Iat: 123})
	w := get(old, "GET", "/api/me")
	c := sessionOf(w)
	if c == nil || c.MaxAge < int((29*24*time.Hour).Seconds()) {
		t.Fatalf("old session not renewed: %+v", c)
	}
	var sc sessionClaims
	if !h.auth.open(c.Value, &sc) || sc.Iat != 123 || time.Until(time.Unix(sc.Exp, 0)) < 29*24*time.Hour {
		t.Fatalf("renewed claims = %+v", sc)
	}
	// Break glass keeps its hour.
	bg := mk(sessionClaims{Email: breakGlassUser, Exp: now.Add(time.Minute).Unix(), BreakGlass: true})
	if w := get(bg, "GET", "/api/me"); sessionOf(w) != nil {
		t.Error("break-glass session renewed")
	}

	// Log out other devices: the caller keeps working, an older cookie
	// (and one from before Iat existed) is void.
	legacy := mk(sessionClaims{Email: "me@example.com", Exp: now.Add(time.Hour).Unix()})
	w = get(fresh, "POST", "/api/logout-others")
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout-others = %d %s", w.Code, w.Body)
	}
	mine := sessionOf(w)
	if mine == nil {
		t.Fatal("caller got no new cookie")
	}
	if w := get(mine, "GET", "/api/devices"); w.Code != 200 {
		t.Errorf("caller after logout-others = %d", w.Code)
	}
	// Another replica (no cache yet) reads the same cut-off from the store.
	h.auth.nbMu.Lock()
	h.auth.nbRead = time.Time{}
	h.auth.nbMu.Unlock()
	for name, c := range map[string]*http.Cookie{"older": mk(sessionClaims{Email: "me@example.com", Exp: now.Add(time.Hour).Unix(), Iat: now.Unix() - 10}), "legacy": legacy} {
		if w := get(c, "GET", "/api/devices"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s cookie after logout-others = %d, want 401", name, w.Code)
		}
	}
	// Not without a session.
	if w := serve(handler, "POST", "/api/logout-others", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("logout-others without login = %d", w.Code)
	}
}
