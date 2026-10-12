package hub

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// hostGuard refuses requests whose Host header names anything but this
// machine's loopback (plus the configured public host). Without it a page
// on any site can DNS-rebind its own name to 127.0.0.1 and become
// same-origin with a hub listening there — cookies and cross-origin checks
// don't help once the origin matches. It matters for the modes whose login
// a rebinding page could satisfy: --no-auth (none), --tailscale-auth (the
// page can set Tailscale-User-Login itself) and the local portal.
func hostGuard(publicURL string, next http.Handler) http.Handler {
	extra := ""
	if u, err := url.Parse(publicURL); err == nil {
		extra = strings.ToLower(u.Hostname())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedHost(r.Host, extra) {
			http.Error(w, "ccdash: unexpected Host header (open the portal as http://127.0.0.1:<port>/)", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedHost reports whether host (a Host header, port optional) is a
// loopback name or extra (a hostname without port; "" = none).
func allowedHost(host, extra string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(h, "["), "]"))
	switch h {
	case "":
		return false
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return extra != "" && h == extra
}
