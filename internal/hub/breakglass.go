package hub

import (
	"crypto/sha256"
	"crypto/subtle"
	"html"
	"log"
	"net/http"
	"strings"
	"time"
)

// Break glass: a password login for when the OIDC provider (Cloudflare
// Access, Google …) is down. It exists only while
// AuthConfig.BreakGlassPassword is set, and only on an OIDC hub. The
// session it gives is short and named breakGlassUser; removing the
// password ends every such session at once (user checks it per request).
const (
	breakGlassPath  = "/auth/break-glass"
	breakGlassUser  = "break-glass"
	breakGlassTTL   = time.Hour
	breakGlassMax   = 5                // failures within breakGlassWin …
	breakGlassWin   = 10 * time.Minute // … lock the door, even to the right password, until they age out
	breakGlassDelay = time.Second      // per failure, so guessing is slow even before the lock
)

func (a *authn) breakGlassEnabled() bool {
	return a.cfg.BreakGlassPassword != "" && a.local == nil && !a.cfg.NoAuth && !a.cfg.TailscaleServe
}

// breakGlassCheck reports whether password opens the door, and why not.
func (a *authn) breakGlassCheck(password string) (bool, string) {
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	now := time.Now()
	kept := a.bgFails[:0]
	for _, t := range a.bgFails {
		if now.Sub(t) < breakGlassWin {
			kept = append(kept, t)
		}
	}
	a.bgFails = kept
	if len(a.bgFails) >= breakGlassMax {
		return false, "locked after repeated failures"
	}
	// Hash both sides so the comparison takes the same time whatever the
	// lengths.
	got, want := sha256.Sum256([]byte(password)), sha256.Sum256([]byte(a.cfg.BreakGlassPassword))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		a.bgFails = append(a.bgFails, now)
		return false, "wrong password"
	}
	return true, ""
}

// clientAddr names the sender for the log: the proxy's X-Forwarded-For
// (as given; it is only a log line) and the direct peer.
func clientAddr(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return f + " via " + r.RemoteAddr
	}
	return r.RemoteAddr
}

func (a *authn) handleBreakGlass(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		breakGlassPage(w, http.StatusOK, "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	who := clientAddr(r)
	ok, why := a.breakGlassCheck(r.PostForm.Get("password"))
	if !ok {
		log.Printf("hub: break-glass: refused from %s: %s", who, why)
		time.Sleep(a.bgDelay)
		breakGlassPage(w, http.StatusForbidden, "Not logged in: "+why+".")
		return
	}
	log.Printf("hub: break-glass: login from %s", who)
	a.setCookie(w, sessionCookie, a.sign(sessionClaims{
		Email:      breakGlassUser,
		Exp:        time.Now().Add(breakGlassTTL).Unix(),
		BreakGlass: true,
	}), breakGlassTTL)
	http.Redirect(w, r, "/", http.StatusFound)
}

func breakGlassPage(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(code)
	var b strings.Builder
	b.WriteString(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>ccdash hub — break glass</title>`)
	b.WriteString(`<body style="font-family:sans-serif;max-width:32rem;margin:2rem auto;padding:0 1rem">`)
	b.WriteString(`<h1>Break glass</h1><p>Only for when the normal login (OIDC) is down. The session lasts one hour.</p>`)
	if msg != "" {
		b.WriteString(`<p><strong>` + html.EscapeString(msg) + `</strong></p>`)
	}
	b.WriteString(`<form method="post" action="` + breakGlassPath + `"><input type="password" name="password" autocomplete="current-password" autofocus required> <button>Log in</button></form>`)
	b.WriteString(`<p><a href="/auth/login">Back to the normal login</a></p>`)
	_, _ = w.Write([]byte(b.String()))
}

// loginFailed answers a failed OIDC login: plain text as before, or, with
// break glass on, a page that links to it.
func (a *authn) loginFailed(w http.ResponseWriter, msg string, code int) {
	if !a.breakGlassEnabled() {
		http.Error(w, msg, code)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>ccdash hub</title>` +
		`<body style="font-family:sans-serif;max-width:32rem;margin:2rem auto;padding:0 1rem">` +
		`<p>` + html.EscapeString(msg) + `</p>` +
		`<p><a href="/auth/login">Try again</a> · <a href="` + breakGlassPath + `">Break glass</a> (if the login provider is down)</p>`))
}
