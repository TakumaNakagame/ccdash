package hub

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// The local portal (`ccdash web`, docs/decisions/0005-local-portal.md):
// the hub's web UI on 127.0.0.1 for this machine only. The one device is
// this machine's collector, linked in-process (ServeLocal); people log in
// with a one-time token from a 0600 file — Jupyter style — instead of
// OIDC.

// LocalConfig configures the local portal.
type LocalConfig struct {
	// LoginFile holds the current login URL (with its one-time token),
	// mode 0600; `ccdash web open` reads it. Rewritten after every use.
	LoginFile string
	// User is shown as the portal's user and keys prefs / quick commands.
	User string
}

// LocalDeviceID is the local portal's one device.
const LocalDeviceID = "local"

const (
	localCookie = "ccdash_web" // not the hub's name: cookies ignore ports
	localTTL    = 30 * 24 * time.Hour
)

func (lc *LocalConfig) check(cfg Config) error {
	if lc.LoginFile == "" || lc.User == "" {
		return errors.New("local portal: login file and user are required")
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Scheme != "http" || !allowedHost(u.Host, "") {
		return fmt.Errorf("local portal: the URL must be http://127.0.0.1:<port>, got %q", cfg.PublicURL)
	}
	if host, _, err := net.SplitHostPort(cfg.Listen); err != nil || !allowedHost(host, "") {
		return fmt.Errorf("local portal: listen address must be loopback, got %q", cfg.Listen)
	}
	if len(cfg.ReadTokens) > 0 {
		return errors.New("local portal: read tokens are not available")
	}
	return nil
}

// localLogin is the current one-time login token.
type localLogin struct {
	cfg  *LocalConfig
	base string // the portal's URL

	mu    sync.Mutex
	token string
}

func newLocalLogin(cfg *LocalConfig, base string) (*localLogin, error) {
	l := &localLogin{cfg: cfg, base: base}
	return l, l.rotate()
}

// URL is the login link for the current token.
func (l *localLogin) URL() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.urlLocked()
}

func (l *localLogin) urlLocked() string {
	return l.base + "/auth/local?token=" + l.token
}

// rotate replaces the token and rewrites the login file (temp + rename,
// so `ccdash web open` never reads half a file).
func (l *localLogin) rotate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.token = randHex(32)
	dir := filepath.Dir(l.cfg.LoginFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".web-login-*")
	if err != nil {
		return err
	}
	_, werr := f.WriteString(l.urlLocked() + "\n")
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(f.Name(), 0o600)); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), l.cfg.LoginFile)
}

// consume reports whether tok is the current token and, if so, rotates it.
func (l *localLogin) consume(tok string) bool {
	l.mu.Lock()
	ok := tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(l.token)) == 1
	l.mu.Unlock()
	if ok {
		if err := l.rotate(); err != nil {
			log.Printf("web: rotate login token: %v", err)
		}
	}
	return ok
}

// LoginURL is the local portal's current login link ("" for a hub).
func (h *Hub) LoginURL() string {
	if h.auth.local == nil {
		return ""
	}
	return h.auth.local.URL()
}

type localClaims struct {
	Exp int64 `json:"x"`
}

// localUser checks the local portal's cookie.
func (a *authn) localUser(r *http.Request) (string, bool) {
	c, err := r.Cookie(localCookie)
	if err != nil {
		return "", false
	}
	var lc localClaims
	if !a.open(c.Value, &lc) || time.Now().Unix() > lc.Exp {
		return "", false
	}
	return a.local.cfg.User, true
}

func (a *authn) setLocalCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     localCookie,
		Value:    a.sign(localClaims{Exp: time.Now().Add(localTTL).Unix()}),
		Path:     "/",
		MaxAge:   int(localTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// renewLocal re-issues a cookie past half its life.
func (a *authn) renewLocal(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(localCookie)
	if err != nil {
		return
	}
	var lc localClaims
	if a.open(c.Value, &lc) && time.Until(time.Unix(lc.Exp, 0)) < localTTL/2 {
		a.setLocalCookie(w)
	}
}

// handleLocalLogin trades the one-time token for the cookie.
func (a *authn) handleLocalLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	next := safeNext(r.URL.Query().Get("next"))
	if a.local.consume(r.URL.Query().Get("token")) {
		log.Printf("web: logged in from a login link")
		a.setLocalCookie(w)
		// Not a 302: a link opened from a file (ccdash web open's redirect
		// page) is a cross-site navigation, and a SameSite=Strict cookie
		// isn't sent anywhere down its redirect chain. This page's own
		// navigation is same-origin, so it is.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=%s"><title>ccdash web</title>`, html.EscapeString(next))
		return
	}
	if _, ok := a.localUser(r); ok {
		// An old link reopened by a browser that is logged in anyway.
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	a.localLoginPage(w, http.StatusForbidden, "このログインリンクは使用済みか、古いものです。")
}

// handleLocalLoginHelp is what a visitor without the cookie sees.
func (a *authn) handleLocalLoginHelp(w http.ResponseWriter, r *http.Request) {
	a.localLoginPage(w, http.StatusUnauthorized, "ログインが必要です。")
}

func (a *authn) localLoginPage(w http.ResponseWriter, code int, why string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><html lang="ja"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`+
		`<title>ccdash web</title><body style="font-family:-apple-system,BlinkMacSystemFont,'Hiragino Sans',sans-serif;max-width:36em;margin:4em auto;padding:0 16px;line-height:1.7">`+
		`<h1 style="font-size:1.3em">ccdash web</h1><p>%s</p>`+
		`<p>このマシンのターミナルで次を実行すると、ログイン用のリンクがブラウザで開きます。</p>`+
		`<pre style="background:#8882;padding:8px 12px;border-radius:8px">ccdash web open</pre></body></html>`, html.EscapeString(why))
}

// ServeLocal makes sess (the hub end of the in-process link to this
// machine's collector) the local portal's device until sess closes.
func (h *Hub) ServeLocal(sess *yamux.Session, hostname, version string) error {
	if h.cfg.Local == nil {
		return errors.New("not a local portal")
	}
	d, err := h.st.ensureLocalDevice(context.Background(), hostname)
	if err != nil {
		return err
	}
	h.serveDevice(d, sess, "local", hostname, version)
	return nil
}

// ensureLocalDevice creates or renames the local portal's one device row.
func (s *store) ensureLocalDevice(ctx context.Context, name string) (Device, error) {
	if !validName(name) {
		name = "this machine"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (id, name, token_hash, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name = excluded.name`,
		LocalDeviceID, name, hashToken(randHex(32)), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return Device{}, err
	}
	d, err := scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, LocalDeviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, errors.New("local device row missing")
	}
	return d, err
}

// pushSubject is the VAPID "sub" claim: a contact for the push service.
// The public URL when it is https; a loopback http URL is not a contact
// (some push services reject it), so the local portal names the project.
func (h *Hub) pushSubject() string {
	if strings.HasPrefix(h.cfg.PublicURL, "https://") {
		return h.cfg.PublicURL
	}
	return "https://github.com/TakumaNakagame/ccdash"
}
