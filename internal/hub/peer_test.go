package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/tunnel"
)

func TestPeerAccess(t *testing.T) {
	h := newTestHub(t, false)
	ctx := context.Background()
	caller, tok, err := h.st.createDevice(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := h.st.createDevice(ctx, "server")
	if err != nil {
		t.Fatal(err)
	}
	dev := attachFakeDevice(t, h, target.ID)
	handler := h.Handler()
	auth := bearer(tok)
	d := "/api/d/" + target.ID

	// Off by default: the token reaches nothing.
	if w := serve(handler, "GET", "/api/devices", "", auth); w.Code != http.StatusForbidden {
		t.Fatalf("peer off: /api/devices = %d, want 403", w.Code)
	}
	if w := serve(handler, "GET", "/api/devices", "", bearer("ccdh_wrong")); w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown device token = %d, want 401", w.Code)
	}

	if err := h.st.setPeerAccess(ctx, caller.ID, PeerRead); err != nil {
		t.Fatal(err)
	}
	w := serve(handler, "GET", "/api/peer/self", "", auth)
	var self struct{ ID, Name, Peer_access string }
	_ = json.Unmarshal(w.Body.Bytes(), &self)
	if w.Code != 200 || self.ID != caller.ID || self.Name != "laptop" || self.Peer_access != PeerRead {
		t.Fatalf("self = %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/api/devices", d + "/api/sessions", d + "/pty/", d + "/hub/info", d + "/api/sessions/abc/transcript", d + "/api/tasks", d + "/api/tasks/11", d + "/api/start-info?project=web"} {
		if w := serve(handler, "GET", p, "", auth); w.Code != 200 {
			t.Errorf("read peer GET %s = %d, want 200", p, w.Code)
		}
	}
	if got := dev.last(); got == nil || got.Header.Get("Authorization") != "" {
		t.Error("the device token must not reach the device")
	}
	// Read access: no writes.
	for _, p := range []string{d + "/pty/start", d + "/api/tasks/11", d + "/api/sessions/abc/parent", d + "/api/sessions/abc/message"} {
		if w := serve(handler, "POST", p, `{}`, auth); w.Code != http.StatusForbidden {
			t.Errorf("read peer POST %s = %d, want 403", p, w.Code)
		}
	}

	if err := h.st.setPeerAccess(ctx, caller.ID, PeerControl); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"POST", d + "/pty/start"}, {"DELETE", d + "/pty/abc"}, {"POST", d + "/api/sessions/abc/title"}, {"POST", d + "/api/sessions/abc/parent"}, {"POST", d + "/api/sessions/abc/message"}, {"POST", d + "/api/restart"},
		{"POST", d + "/api/tasks"}, {"POST", d + "/api/tasks/11"}, {"POST", d + "/api/tasks/11/notes"}, {"POST", d + "/api/tasks/order"}} {
		if w := serve(handler, c[0], c[1], `{}`, auth); w.Code != 200 {
			t.Errorf("control peer %s %s = %d, want 200", c[0], c[1], w.Code)
		}
	}
	// A peer's start can't ask for an orchestrator role — with any
	// spelling of the field — and an ordinary start still reaches the
	// device whole.
	for _, body := range []string{`{"cwd":"/tmp","role":"orchestrator"}`, `{"cwd":"/tmp","Role":"project-orchestrator","project":"web"}`, `{"role":"x","cwd":"/tmp"}`} {
		if w := serve(handler, "POST", d+"/pty/start", body, auth); w.Code != http.StatusForbidden {
			t.Errorf("control peer start %s = %d, want 403", body, w.Code)
		}
	}
	if w := serve(handler, "POST", d+"/pty/start", `{"cwd":"/tmp","prompt":"hello","role":""}`, auth); w.Code != 200 {
		t.Errorf("control peer plain start = %d, want 200", w.Code)
	}
	// Never: device management, approvals, typing, the raw terminal,
	// settings, other hub routes.
	for _, c := range [][2]string{
		{"POST", "/api/devices"}, {"POST", "/api/devices/" + caller.ID + "/peer"}, {"DELETE", "/api/devices/" + target.ID},
		{"POST", d + "/approvals/1/decide"}, {"POST", d + "/pty/abc/input"}, {"GET", d + "/pty/abc/stream"},
		{"GET", d + "/ws/pty/abc"}, {"POST", d + "/api/settings"}, {"GET", d + "/api/settings"}, {"POST", d + "/api/sessions/abc/wait"},
		{"GET", d + "/api/sessions/abc/brief"}, {"POST", d + "/api/sessions/abc/children-seen"}, {"GET", d + "/api/peers"},
		{"GET", d + "/api/sessions/abc/message"}, {"POST", d + "/api/sessions/abc/message/1"}, {"POST", d + "/api/sessions/abc/messages"},
		{"POST", d + "/api/tasks/sync"}, {"POST", d + "/api/tasks/import"}, {"POST", d + "/api/tasks/PROJ-1"},
		{"GET", "/api/board"}, {"GET", "/api/prefs"}, {"POST", "/api/push/test"}, {"GET", d + "/hub/file"},
		// The operator's notes for start_session briefs and a project's
		// directory are a person's to edit: a device token changes none of
		// them (nor reads the common note), not even with control access.
		{"PUT", d + "/api/brief-common"}, {"POST", d + "/api/brief-common"}, {"GET", d + "/api/brief-common"},
		{"POST", d + "/api/projects/brief"}, {"POST", d + "/api/projects/dir"},
		{"PUT", d + "/api/settings/session_setup_command"}, {"PUT", d + "/api/settings/brief_common"},
		// Roles are the operator's to give: not the route, not the
		// project's orchestrator model, not the orchestrator's own text.
		{"POST", d + "/api/sessions/abc/role"}, {"POST", d + "/api/projects/orchestrator"},
		{"PUT", d + "/api/settings/orchestrator_prompt"}, {"POST", d + "/pty/abc/restart"},
	} {
		if w := serve(handler, c[0], c[1], `{}`, auth); w.Code != http.StatusForbidden {
			t.Errorf("control peer %s %s = %d, want 403", c[0], c[1], w.Code)
		}
	}
}

func TestPeerAccessRouteNeedsLogin(t *testing.T) {
	h := newTestHub(t, false)
	d, _, err := h.st.createDevice(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if w := serve(h.Handler(), "POST", "/api/devices/"+d.ID+"/peer", `{"access":"control"}`, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous peer toggle = %d, want 401", w.Code)
	}
	h2 := newTestHub(t, true)
	d2, _, _ := h2.st.createDevice(context.Background(), "y")
	if w := serve(h2.Handler(), "POST", "/api/devices/"+d2.ID+"/peer", `{"access":"bogus"}`, nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad access = %d, want 400", w.Code)
	}
	if w := serve(h2.Handler(), "POST", "/api/devices/"+d2.ID+"/peer", `{"access":"read"}`, nil); w.Code != http.StatusNoContent {
		t.Errorf("set access = %d, want 204", w.Code)
	}
	ds, _ := h2.st.listDevices(context.Background())
	if len(ds) != 1 || ds[0].PeerAccess != PeerRead {
		t.Errorf("stored = %+v", ds)
	}
}

// TestPeerIdentityHeaders: the device a peer request comes from is named to
// the target device by the hub, from the token — whatever the request
// itself claims in those headers or in its body — and a person's request
// (the portal) never carries the headers at all, even when it sends them.
func TestPeerIdentityHeaders(t *testing.T) {
	ctx := context.Background()
	h := newTestHub(t, true) // --no-auth: every request is the logged-in operator
	caller, tok, err := h.st.createDevice(ctx, "仕事 laptop")
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := h.st.createDevice(ctx, "server")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.setPeerAccess(ctx, caller.ID, PeerControl); err != nil {
		t.Fatal(err)
	}
	dev := attachFakeDevice(t, h, target.ID)
	handler := h.Handler()
	path := "/api/d/" + target.ID + "/api/sessions/abc/message"
	forged := map[string]string{
		tunnel.HeaderPeerID: "someone-else", tunnel.HeaderPeerName: "operator", tunnel.HeaderPeerLetter: "Z",
	}
	body := `{"text":"hi","from_session":"s1","from_host":"operator"}`

	// A peer: the hub's own values replace the forged ones.
	hdr := bearer(tok)
	for k, v := range forged {
		hdr[k] = v
	}
	if w := serve(handler, "POST", path, body, hdr); w.Code != 200 {
		t.Fatalf("peer message = %d %s", w.Code, w.Body)
	}
	got := dev.last()
	if got == nil {
		t.Fatal("the request did not reach the device")
	}
	name, _ := url.QueryUnescape(got.Header.Get(tunnel.HeaderPeerName))
	if got.Header.Get(tunnel.HeaderPeerID) != caller.ID || name != "仕事 laptop" || got.Header.Get(tunnel.HeaderPeerLetter) != caller.Letter {
		t.Errorf("peer headers = %q %q %q, want the caller device %q / its name / %q",
			got.Header.Get(tunnel.HeaderPeerID), name, got.Header.Get(tunnel.HeaderPeerLetter), caller.ID, caller.Letter)
	}
	if caller.Letter == "" {
		t.Error("the caller device has no letter")
	}
	if len(got.Header.Values(tunnel.HeaderPeerID)) != 1 {
		t.Errorf("more than one peer id header: %q", got.Header.Values(tunnel.HeaderPeerID))
	}

	// A person in the portal (here: the --no-auth operator): the headers
	// they send are dropped, so the device sees no peer.
	if w := serve(handler, "POST", path, body, forged); w.Code != 200 {
		t.Fatalf("portal request = %d %s", w.Code, w.Body)
	}
	got = dev.last()
	for _, k := range []string{tunnel.HeaderPeerID, tunnel.HeaderPeerName, tunnel.HeaderPeerLetter} {
		if v := got.Header.Get(k); v != "" {
			t.Errorf("portal request reached the device with %s = %q", k, v)
		}
	}
}
