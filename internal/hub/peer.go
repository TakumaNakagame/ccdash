package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
)

// Peer access lets one joined device reach the other devices through the
// hub with its own device token (`Authorization: Bearer ccdh_…`), so that
// `ccdash mcp` on it can list and drive sessions on every host. It is off
// per device until the operator turns it on in the portal ("read" or
// "control"); turning it off again cuts it at the next request. A peer
// reaches only the hub's device list, who it is (/api/peer/self) and a
// narrow set of device routes (peerDeviceRoute) — never device
// management, push, prefs, the raw terminal, typing into a session or
// answering approvals. The target device's own allowlist (hubAllowed, with
// its attach_enabled gate) still applies on top. See
// docs/decisions/0007-multihost.md.

// Peer access levels (devices.peer_access).
const (
	PeerOff     = ""
	PeerRead    = "read"
	PeerControl = "control"
)

func validPeerAccess(v string) bool { return v == PeerOff || v == PeerRead || v == PeerControl }

// deviceTokenPrefix marks a hub device token (createDevice); read tokens
// are operator-chosen strings, so a bearer with this prefix is a peer.
const deviceTokenPrefix = "ccdh_"

type peerRoute struct {
	method string
	re     *regexp.Regexp
	write  bool // needs PeerControl
}

// peerDeviceRoutes is what a peer may ask of a device (the path after
// /api/d/{id}). Reads: sessions, a transcript tail, approvals (to show
// what is pending — never to decide), projects, usage, the restart state,
// the hosted PTYs, the device's info and its directory listing. Writes:
// the session edits the portal makes, starting / stopping a hosted claude
// and the (scheduled) collector restart.
var peerDeviceRoutes = []peerRoute{
	{"GET", regexp.MustCompile(`^/api/(sessions|approvals|projects|usage|restart)$`), false},
	{"GET", regexp.MustCompile(`^/api/sessions/[^/]+/transcript$`), false},
	{"GET", regexp.MustCompile(`^/pty/?$`), false},
	{"GET", regexp.MustCompile(`^/hub/(info|dirs)$`), false},
	{"POST", regexp.MustCompile(`^/api/sessions/[^/]+/(archive|favorite|later|grid|color|title|project|seen)$`), true},
	{"POST", regexp.MustCompile(`^/api/projects/(color|rename|order)$`), true},
	{"POST", regexp.MustCompile(`^/pty/start$`), true},
	{"DELETE", regexp.MustCompile(`^/pty/[^/]+$`), true},
	{"POST", regexp.MustCompile(`^/api/restart$`), true},
	{"DELETE", regexp.MustCompile(`^/api/restart$`), true},
}

var peerProxyPath = regexp.MustCompile(`^/api/d/[^/]+(/.*)$`)

// peerAllowed reports whether a peer with access level may make r.
func peerAllowed(r *http.Request, access string) (bool, string) {
	if access == PeerOff {
		return false, "peer access is off for this device (turn it on in the hub portal: 端末 → … → 他の端末へのアクセス)"
	}
	p := r.URL.Path
	if r.Method == http.MethodGet && (p == "/api/devices" || p == "/api/peer/self") {
		return true, ""
	}
	m := peerProxyPath.FindStringSubmatch(p)
	if m == nil || strings.Contains(p, "/ws/") {
		return false, "not available to peers"
	}
	for _, rt := range peerDeviceRoutes {
		if rt.method == r.Method && rt.re.MatchString(m[1]) {
			if rt.write && access != PeerControl {
				return false, "this device has read-only peer access"
			}
			return true, ""
		}
	}
	return false, "not available to peers"
}

// servePeer handles a bearer request carrying a device token.
func (h *Hub) servePeer(w http.ResponseWriter, r *http.Request, tok string, api http.Handler) {
	d, err := h.st.deviceByToken(r.Context(), tok)
	if err != nil {
		log.Printf("hub: rejected device token from %s (%s %s)", clientIP(r), r.Method, r.URL.Path)
		w.Header().Set("WWW-Authenticate", `Bearer realm="ccdash-hub"`)
		http.Error(w, "unknown device token", http.StatusUnauthorized)
		return
	}
	if ok, why := peerAllowed(r, d.PeerAccess); !ok {
		http.Error(w, why, http.StatusForbidden)
		return
	}
	if r.URL.Path == "/api/peer/self" {
		writeJSON(w, map[string]any{"id": d.ID, "name": d.Name, "peer_access": d.PeerAccess})
		return
	}
	if r.Method != http.MethodGet {
		log.Printf("hub: peer %q: %s %s", d.Name, r.Method, r.URL.Path)
	}
	r.Header.Del("Cookie")
	api.ServeHTTP(w, r)
}

// handlePeerAccess sets a device's peer access (a logged-in operator only:
// peers never reach this route).
func (h *Hub) handlePeerAccess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Access string `json:"access"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || !validPeerAccess(req.Access) {
		http.Error(w, `access must be "", "read" or "control"`, http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	if err := h.st.setPeerAccess(r.Context(), id, req.Access); errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	email, _ := h.auth.user(r)
	log.Printf("hub: %s set peer access of device %s to %q", email, id, req.Access)
	w.WriteHeader(http.StatusNoContent)
}
