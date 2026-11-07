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
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// AuthConfig selects how portal users log in. The hub is an OIDC relying
// party; any OIDC issuer works (Cloudflare Access for SaaS, Google, a
// self-hosted Authelia / Pocket ID …).
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
	// BreakGlassPassword, when set on an OIDC hub, opens a password login
	// at /auth/break-glass for when the provider is down (breakglass.go).
	BreakGlassPassword string
	// SessionTTL is how long a login lasts without use (0 = DefaultSessionTTL).
	// The portal and the app poll /api/me, which slides the expiry forward
	// once half of it has passed, so a hub in daily use stays logged in;
	// "log out other devices" (POST /api/logout-others) ends the rest.
	SessionTTL time.Duration
}

// DefaultSessionTTL is the hub login's lifetime when --session-ttl is unset;
// MaxSessionTTL caps the flag.
const (
	DefaultSessionTTL = 24 * time.Hour
	MaxSessionTTL     = 90 * 24 * time.Hour
)

// tailscaleLoginHeader is the identity header `tailscale serve` adds.
const tailscaleLoginHeader = "Tailscale-User-Login"

const (
	sessionCookie = "ccdash_hub"
	// loginCookie prefixes one cookie per in-flight login, named by its
	// state: an expired session makes every open tab (and the installed
	// PWA) start a login at once, and with a single cookie each one
	// overwrote the others' state, so all but the last callback failed
	// with "state mismatch".
	loginCookie = "ccdash_hub_login_"
	loginPath   = "/auth/"
	loginTTL    = 10 * time.Minute
	// reloginCookie marks a login the hub restarted by itself because the
	// callback came back after its state had expired (a login page left
	// open on the phone, then finished hours later). Only one restart per
	// reloginTTL, so a browser that drops cookies can't loop.
	reloginCookie = "ccdash_hub_relogin"
	reloginTTL    = 2 * time.Minute
	// notBeforeKey is the kv row holding the Unix time before which
	// sessions are void ("log out other devices"); notBeforeCache is how
	// long a replica trusts its copy.
	notBeforeKey   = "session_not_before"
	notBeforeCache = 5 * time.Second
)

type authn struct {
	cfg       AuthConfig
	publicURL string      // origin the browser uses; the OIDC redirect URI hangs off it
	key       []byte      // HMAC key for both cookies
	secure    bool        // set the Secure cookie flag (https public URL)
	local     *localLogin // the local portal's login (local.go); nil on a hub

	mu       sync.Mutex
	provider *oidc.Provider // lazily discovered so a provider blip doesn't block startup

	bgMu    sync.Mutex
	bgFails []time.Time   // recent break-glass failures (per replica)
	bgDelay time.Duration // pause after a failure

	// st holds notBeforeKey (nil in tests that build an authn by hand:
	// nothing is ever revoked there).
	st     stateStore
	nbMu   sync.Mutex
	nbVal  int64
	nbRead time.Time
}

func (a *authn) sessionTTL() time.Duration {
	if a.cfg.SessionTTL > 0 {
		return a.cfg.SessionTTL
	}
	return DefaultSessionTTL
}

// notBefore is the Unix time before which a session cookie no longer
// counts, read through a short cache (every API call checks it).
func (a *authn) notBefore(ctx context.Context) int64 {
	if a.st == nil {
		return 0
	}
	a.nbMu.Lock()
	defer a.nbMu.Unlock()
	if time.Since(a.nbRead) < notBeforeCache {
		return a.nbVal
	}
	v, err := a.st.secret(ctx, notBeforeKey, func() string { return "0" })
	if err != nil {
		log.Printf("hub: read %s: %v", notBeforeKey, err)
		return a.nbVal // keep the last known value
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	a.nbVal, a.nbRead = n, time.Now()
	return n
}

// newSession sets the login cookie for email, issued at iat.
func (a *authn) newSession(w http.ResponseWriter, email string, iat int64) {
	ttl := a.sessionTTL()
	a.setCookie(w, sessionCookie, a.sign(sessionClaims{
		Email: email,
		Exp:   time.Now().Add(ttl).Unix(),
		Iat:   iat,
	}), ttl)
}

// session returns the valid session claims on r, if any.
func (a *authn) session(r *http.Request) (sessionClaims, bool) {
	var sc sessionClaims
	c, err := r.Cookie(sessionCookie)
	if err != nil || !a.open(c.Value, &sc) || time.Now().Unix() > sc.Exp {
		return sc, false
	}
	if sc.Iat < a.notBefore(r.Context()) {
		return sc, false
	}
	return sc, true
}

// renewSession slides an OIDC session forward once half of its lifetime
// has passed (the portal polls /api/me). Break-glass sessions keep their
// hour.
func (a *authn) renewSession(w http.ResponseWriter, r *http.Request) {
	if a.local != nil || a.cfg.NoAuth || a.cfg.TailscaleServe {
		return
	}
	sc, ok := a.session(r)
	if !ok || sc.BreakGlass || !a.allowed(sc.Email) {
		return
	}
	if time.Until(time.Unix(sc.Exp, 0)) < a.sessionTTL()/2 {
		a.newSession(w, sc.Email, sc.Iat)
	}
}

// handleLogoutOthers voids every session but the caller's: a lost phone's
// login, say. The caller gets a fresh cookie issued at the cut-off.
func (a *authn) handleLogoutOthers(w http.ResponseWriter, r *http.Request) {
	sc, ok := a.session(r)
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	now := time.Now().Unix()
	if err := a.st.putKV(r.Context(), notBeforeKey, strconv.FormatInt(now, 10)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.nbMu.Lock()
	a.nbVal, a.nbRead = now, time.Now()
	a.nbMu.Unlock()
	log.Printf("hub: %s logged out every other session", sc.Email)
	if sc.BreakGlass {
		// Keep the break-glass hour rather than turning it into a full login.
		a.setCookie(w, sessionCookie, a.sign(sessionClaims{Email: sc.Email, Exp: sc.Exp, Iat: now, BreakGlass: true}), time.Until(time.Unix(sc.Exp, 0)))
	} else {
		a.newSession(w, sc.Email, now)
	}
	w.WriteHeader(http.StatusNoContent)
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
	// Iat is when the login happened (0 on cookies from before it was
	// recorded); a session issued before notBeforeKey is void.
	Iat int64 `json:"i,omitempty"`
	// BreakGlass marks a session from the break-glass login: valid only
	// while the password is still configured.
	BreakGlass bool `json:"bg,omitempty"`
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
	if a.local != nil {
		return a.localUser(r)
	}
	if a.cfg.NoAuth {
		return "dev@localhost", true
	}
	if a.cfg.TailscaleServe {
		login := strings.ToLower(strings.TrimSpace(r.Header.Get(tailscaleLoginHeader)))
		return login, a.allowed(login)
	}
	sc, ok := a.session(r)
	if !ok {
		return "", false
	}
	if sc.BreakGlass {
		return breakGlassUser, a.breakGlassEnabled()
	}
	if !a.allowed(sc.Email) {
		return "", false
	}
	return sc.Email, true
}

func (a *authn) allowed(email string) bool {
	return email != "" && slices.Contains(a.cfg.AllowedEmails, strings.ToLower(email))
}

func (a *authn) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	a.setCookiePath(w, name, value, "/", ttl)
}

func (a *authn) setCookiePath(w http.ResponseWriter, name, value, path string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *authn) clearCookie(w http.ResponseWriter, name string) {
	a.clearCookiePath(w, name, "/")
}

func (a *authn) clearCookiePath(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
}

// safeNext keeps post-login redirects on this origin. Browsers drop tabs
// and newlines from a URL and read a backslash as '/', so "/\t/evil" would become
// "//evil": any control character or backslash is refused outright.
func safeNext(next string) string {
	if next == "" || next[0] != '/' || (len(next) > 1 && (next[1] == '/' || next[1] == '\\')) {
		return "/"
	}
	if strings.ContainsFunc(next, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Scheme != "" || u.Host != "" {
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
		a.loginFailed(w, "login provider unavailable, try again shortly", http.StatusServiceUnavailable)
		return
	}
	ls := loginState{
		State:    randHex(16),
		Nonce:    randHex(16),
		Verifier: oauth2.GenerateVerifier(),
		Next:     safeNext(r.URL.Query().Get("next")),
		Exp:      time.Now().Add(loginTTL).Unix(),
	}
	a.setCookiePath(w, loginCookie+ls.State, a.sign(ls), loginPath, loginTTL)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, oc.AuthCodeURL(ls.State, oidc.Nonce(ls.Nonce), oauth2.S256ChallengeOption(ls.Verifier)), http.StatusFound)
}

func (a *authn) handleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	var ls loginState
	c, err := r.Cookie(loginCookie + state)
	if state == "" || err != nil || !a.open(c.Value, &ls) || time.Now().Unix() > ls.Exp || ls.State != state {
		// A tab whose login was overtaken by another one that already
		// finished: the browser is logged in, so just go back.
		if _, ok := a.user(r); ok {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		// The login page sat open past its state's lifetime (a phone left
		// on the provider's page): start a fresh login instead of an
		// error, once per reloginTTL so a cookie-less browser can't loop.
		if _, err := r.Cookie(reloginCookie); err != nil && r.URL.Query().Get("error") == "" {
			log.Printf("hub: login state expired or missing; starting a new login")
			a.setCookiePath(w, reloginCookie, "1", loginPath, reloginTTL)
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "/auth/login?next=/", http.StatusFound)
			return
		}
		a.loginFailed(w, "login expired — start again from the portal", http.StatusBadRequest)
		return
	}
	a.clearCookiePath(w, loginCookie+state, loginPath)
	if e := r.URL.Query().Get("error"); e != "" {
		a.loginFailed(w, "login failed: "+e, http.StatusUnauthorized)
		return
	}
	oc, verifier, err := a.oauth(r.Context())
	if err != nil {
		a.loginFailed(w, "login provider unavailable", http.StatusServiceUnavailable)
		return
	}
	tok, err := oc.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		log.Printf("hub: oidc exchange: %v", err)
		a.loginFailed(w, "login failed (code exchange)", http.StatusUnauthorized)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		a.loginFailed(w, "login failed (no id_token)", http.StatusUnauthorized)
		return
	}
	idt, err := verifier.Verify(r.Context(), raw)
	if err != nil {
		log.Printf("hub: oidc verify: %v", err)
		a.loginFailed(w, "login failed (id_token)", http.StatusUnauthorized)
		return
	}
	if idt.Nonce != ls.Nonce {
		a.loginFailed(w, "login failed (nonce)", http.StatusUnauthorized)
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
	}
	if err := idt.Claims(&claims); err != nil || claims.Email == "" {
		a.loginFailed(w, "login failed (no email claim)", http.StatusUnauthorized)
		return
	}
	if claims.EmailVerified != nil && !*claims.EmailVerified {
		a.loginFailed(w, "login failed (email not verified)", http.StatusUnauthorized)
		return
	}
	if !a.allowed(claims.Email) {
		log.Printf("hub: refused login for %s (not in allowed emails)", claims.Email)
		http.Error(w, claims.Email+" is not allowed on this hub", http.StatusForbidden)
		return
	}
	log.Printf("hub: login %s", claims.Email)
	a.clearCookiePath(w, reloginCookie, loginPath)
	a.newSession(w, strings.ToLower(claims.Email), time.Now().Unix())
	http.Redirect(w, r, ls.Next, http.StatusFound)
}

func (a *authn) handleLogout(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, sessionCookie)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>ccdash hub</title>` +
		`<p style="font-family:sans-serif">Logged out. <a href="/auth/login">Log in again</a></p>`))
}

var errNoAllowedEmails = errors.New("refusing to start: no allowed emails configured (--allowed-email)")
