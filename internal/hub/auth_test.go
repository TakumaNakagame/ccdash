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

	// A callback without the login cookie (CSRF / replay) is refused.
	if w := serve(handler, "GET", "/auth/callback?code=good-code&state=x", "", nil); w.Code != http.StatusBadRequest {
		t.Errorf("callback without login cookie = %d, want 400", w.Code)
	}
}
