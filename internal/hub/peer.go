package hub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/TakumaNakagame/ccdash/internal/tunnel"
)

// Peer access lets one joined device reach the other devices through the
// hub with its own device token (`Authorization: Bearer ccdh_…`), so that
// `ccdash mcp` on it can list and drive sessions on every host. It is off
// per device until the operator turns it on in the portal ("read" or
// "control"); turning it off again cuts it at the next request. A peer
// reaches only the hub's device list, who it is (/api/peer/self) and a
// narrow set of device routes (peerDeviceRoute) — never device
// management, push, prefs, the raw terminal, typing into a session or
// answering approvals. The one thing a peer may say to a session is a
// message from its parent or child (tell_session), which the target
// collector labels and delivers itself. The target device's own allowlist (hubAllowed, with
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
// the hosted PTYs, the device's info, its directory listing and its task
// list. Writes: the session edits the portal makes (and a session's parent,
// so adopt_session reaches a child on another host), starting / stopping a
// hosted claude, the (scheduled) collector restart and the person-sized
// task edits (add, patch, note, order — never sync or import, and only by
// numeric id), and a message between a parent and a child session. /api/start-info is start_session's question before a start
// that names no directory (the project's, and whether a setup command
// runs). Deliberately absent: the operator's notes for start_session
// briefs and a project's directory (/api/brief-common,
// /api/projects/brief|dir) — they are input to every child session, so
// only a logged-in person edits them, never a device token.
var peerDeviceRoutes = []peerRoute{
	{"GET", regexp.MustCompile(`^/api/(sessions|approvals|projects|usage|restart|start-info|login)$`), false},
	{"GET", regexp.MustCompile(`^/api/sessions/[^/]+/transcript$`), false},
	{"GET", regexp.MustCompile(`^/pty/?$`), false},
	{"GET", regexp.MustCompile(`^/hub/(info|dirs)$`), false},
	{"GET", regexp.MustCompile(`^/api/tasks(/[^/]+)?$`), false},
	{"POST", regexp.MustCompile(`^/api/sessions/[^/]+/(archive|favorite|later|grid|color|title|project|parent|seen)$`), true},
	// A message from one of the peer's sessions to its child or parent
	// here (tell_session). Not typing: the target collector writes the
	// header naming the sender (setPeerHeaders), checks the two sessions
	// are parent and child, and delivers only when no dialog is up.
	{"POST", regexp.MustCompile(`^/api/sessions/[^/]+/message$`), true},
	{"POST", regexp.MustCompile(`^/api/projects/(color|rename|order)$`), true},
	{"POST", regexp.MustCompile(`^/api/tasks(/order|/[0-9]+(/notes)?)?$`), true},
	{"POST", regexp.MustCompile(`^/pty/start$`), true},
	{"DELETE", regexp.MustCompile(`^/pty/[^/]+$`), true},
	{"POST", regexp.MustCompile(`^/api/restart$`), true},
	{"DELETE", regexp.MustCompile(`^/api/restart$`), true},
}

var peerProxyPath = regexp.MustCompile(`^/api/d/[^/]+(/.*)$`)

// peerAllowed reports whether a peer with access level may make r.
func peerAllowed(r *http.Request, access string) (bool, string) {
	if access == PeerOff {
		return false, "peer access is off for this device (turn it on in the hub portal: Devices → Manage → Access to other devices / 端末 → 管理 → 他の端末へのアクセス)"
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
		writeJSON(w, map[string]any{"id": d.ID, "name": d.Name, "peer_access": d.PeerAccess, "letter": d.Letter})
		return
	}
	if r.Method != http.MethodGet {
		log.Printf("hub: peer %q: %s %s", d.Name, r.Method, r.URL.Path)
	}
	if ok, why := peerStartOK(r); !ok {
		http.Error(w, why, http.StatusForbidden)
		return
	}
	r.Header.Del("Cookie")
	api.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerKey{}, &d)))
}

// peerStartMax bounds the /pty/start body the hub reads to look for a role.
const peerStartMax = 4 << 20

var peerStartPath = regexp.MustCompile(`^/api/d/[^/]+/pty/start$`)

// peerStartOK refuses a peer's start that asks for an orchestrator role:
// only the operator makes a session an orchestrator (the role route is not
// a peer route either). The target collector refuses it too, by the peer
// headers; the hub is the one that knows for certain who is a peer. The
// body is put back for the proxy.
func peerStartOK(r *http.Request) (bool, string) {
	if r.Method != http.MethodPost || !peerStartPath.MatchString(r.URL.Path) || r.Body == nil {
		return true, ""
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, peerStartMax+1))
	_ = r.Body.Close()
	if err != nil || len(b) > peerStartMax {
		return false, "the start request is too large"
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	var body struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(b, &body) == nil && strings.TrimSpace(body.Role) != "" {
		return false, "another device can't start an orchestrator: only the operator gives a session a role"
	}
	return true, ""
}

// peerKey carries the calling device of a peer request to the device
// proxy, which names it to the target device (tunnel.HeaderPeerID …).
type peerKey struct{}

func peerOf(ctx context.Context) *Device {
	d, _ := ctx.Value(peerKey{}).(*Device)
	return d
}

// setPeerHeaders replaces the peer-identity headers of a request bound for
// a device: dropped for a person's request (d == nil) whatever the client
// sent, set from the authenticated device for a peer's. The target
// collector trusts them for the sender of a message between sessions
// (POST /api/sessions/{id}/message) and refuses that route without them,
// which is also what keeps it from the portal.
func setPeerHeaders(h http.Header, d *Device) {
	h.Del(tunnel.HeaderPeerID)
	h.Del(tunnel.HeaderPeerName)
	h.Del(tunnel.HeaderPeerLetter)
	if d == nil {
		return
	}
	h.Set(tunnel.HeaderPeerID, d.ID)
	h.Set(tunnel.HeaderPeerName, url.QueryEscape(d.Name))
	h.Set(tunnel.HeaderPeerLetter, d.Letter)
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
