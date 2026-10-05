package hub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// AuthConfig selects how portal users log in. The hub is an OIDC relying
// party; in the home lab the provider is Cloudflare Access for SaaS (the
// same pattern Grafana / ArgoCD use there), but any OIDC issuer works.
type AuthConfig struct {
	Issuer        string // e.g. https://<team>.cloudflareaccess.com/cdn-cgi/access/sso/oidc/<client-id>
	ClientID      string
	ClientSecret  string
	AllowedEmails []string // lower-cased; a login outside this list is refused
	// NoAuth skips login entirely (every request is "dev@localhost"). Only
	// for local development; the CLI refuses it on a non-loopback bind.
	NoAuth bool
	// TailscaleServe trusts the Tailscale-User-Login header that
	// `tailscale serve` sets (and strips from client requests) on what it
	// proxies to a loopback listener; the login must be in AllowedEmails.
	// The CLI requires a loopback --listen for this mode, so only the
	// local tailscaled can reach the hub.
	TailscaleServe bool
}

// tailscaleLoginHeader is the identity header `tailscale serve` adds.
const tailscaleLoginHeader = "Tailscale-User-Login"

const (
	sessionCookie = "ccdash_hub"
	loginCookie   = "ccdash_hub_login"
	sessionTTL    = 24 * time.Hour
	loginTTL      = 10 * time.Minute
)

type authn struct {
	cfg       AuthConfig
	publicURL string // origin the browser uses; the OIDC redirect URI hangs off it
	key       []byte // HMAC key for both cookies
	secure    bool   // set the Secure cookie flag (https public URL)

	mu       sync.Mutex
	provider *oidc.Provider // lazily discovered so a provider blip doesn't block startup
}

func (a *authn) oauth(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider == nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		p, err := oidc.NewProvider(ctx, a.cfg.Issuer)
		if err != nil {
			return nil, nil, err
		}
		a.provider = p
	}
	oc := &oauth2.Config{
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.ClientSecret,
		Endpoint:     a.provider.Endpoint(),
		RedirectURL:  a.publicURL + "/auth/callback",
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
	return oc, a.provider.Verifier(&oidc.Config{ClientID: a.cfg.ClientID}), nil
}

// sign / open implement a tiny signed-cookie format: base64(json).base64(hmac).
func (a *authn) sign(v any) string {
	b, _ := json.Marshal(v)
	body := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *authn) open(s string, v any) bool {
	body, mac, ok := strings.Cut(s, ".")
	if !ok {
		return false
	}
	want := hmac.New(sha256.New, a.key)
	want.Write([]byte(body))
	got, err := base64.RawURLEncoding.DecodeString(mac)
	if err != nil || !hmac.Equal(got, want.Sum(nil)) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

type sessionClaims struct {
	Email string `json:"e"`
	Exp   int64  `json:"x"`
}

type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"t"`
	Exp      int64  `json:"x"`
}

// user returns the logged-in email for r.
func (a *authn) user(r *http.Request) (string, bool) {
	if a.cfg.NoAuth {
		return "dev@localhost", true
	}
	if a.cfg.TailscaleServe {
		login := strings.ToLower(strings.TrimSpace(r.Header.Get(tailscaleLoginHeader)))
		return login, a.allowed(login)
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	var sc sessionClaims
	if !a.open(c.Value, &sc) || time.Now().Unix() > sc.Exp || !a.allowed(sc.Email) {
		return "", false
	}
	return sc.Email, true
}

func (a *authn) allowed(email string) bool {
	return email != "" && slices.Contains(a.cfg.AllowedEmails, strings.ToLower(email))
}

func (a *authn) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *authn) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
}

// safeNext keeps post-login redirects on this origin.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func (a *authn) handleLogin(w http.ResponseWriter, r *http.Request) {
	if a.cfg.NoAuth || a.cfg.TailscaleServe {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	oc, _, err := a.oauth(r.Context())
	if err != nil {
		log.Printf("hub: oidc discovery: %v", err)
		http.Error(w, "login provider unavailable, try again shortly", http.StatusServiceUnavailable)
		return
	}
	ls := loginState{
		State:    randHex(16),
		Nonce:    randHex(16),
		Verifier: oauth2.GenerateVerifier(),
		Next:     safeNext(r.URL.Query().Get("next")),
		Exp:      time.Now().Add(loginTTL).Unix(),
	}
	a.setCookie(w, loginCookie, a.sign(ls), loginTTL)
	http.Redirect(w, r, oc.AuthCodeURL(ls.State, oidc.Nonce(ls.Nonce), oauth2.S256ChallengeOption(ls.Verifier)), http.StatusFound)
}

func (a *authn) handleCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(loginCookie)
	var ls loginState
	if err != nil || !a.open(c.Value, &ls) || time.Now().Unix() > ls.Exp {
		http.Error(w, "login expired — start again from the portal", http.StatusBadRequest)
		return
	}
	a.clearCookie(w, loginCookie)
	if r.URL.Query().Get("state") != ls.State {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "login failed: "+e, http.StatusUnauthorized)
		return
	}
	oc, verifier, err := a.oauth(r.Context())
	if err != nil {
		http.Error(w, "login provider unavailable", http.StatusServiceUnavailable)
		return
	}
	tok, err := oc.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		log.Printf("hub: oidc exchange: %v", err)
		http.Error(w, "login failed (code exchange)", http.StatusUnauthorized)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		http.Error(w, "login failed (no id_token)", http.StatusUnauthorized)
		return
	}
	idt, err := verifier.Verify(r.Context(), raw)
	if err != nil {
		log.Printf("hub: oidc verify: %v", err)
		http.Error(w, "login failed (id_token)", http.StatusUnauthorized)
		return
	}
	if idt.Nonce != ls.Nonce {
		http.Error(w, "login failed (nonce)", http.StatusUnauthorized)
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
	}
	if err := idt.Claims(&claims); err != nil || claims.Email == "" {
		http.Error(w, "login failed (no email claim)", http.StatusUnauthorized)
		return
	}
	if claims.EmailVerified != nil && !*claims.EmailVerified {
		http.Error(w, "login failed (email not verified)", http.StatusUnauthorized)
		return
	}
	if !a.allowed(claims.Email) {
		log.Printf("hub: refused login for %s (not in allowed emails)", claims.Email)
		http.Error(w, claims.Email+" is not allowed on this hub", http.StatusForbidden)
		return
	}
	log.Printf("hub: login %s", claims.Email)
	a.setCookie(w, sessionCookie, a.sign(sessionClaims{
		Email: strings.ToLower(claims.Email),
		Exp:   time.Now().Add(sessionTTL).Unix(),
	}), sessionTTL)
	http.Redirect(w, r, ls.Next, http.StatusFound)
}

func (a *authn) handleLogout(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, sessionCookie)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>ccdash hub</title>` +
		`<p style="font-family:sans-serif">Logged out. <a href="/auth/login">Log in again</a></p>`))
}

var errNoAllowedEmails = errors.New("refusing to start: no allowed emails configured (--allowed-email)")
