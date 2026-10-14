package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
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
	for _, p := range []string{"/api/devices", d + "/api/sessions", d + "/pty/", d + "/hub/info", d + "/api/sessions/abc/transcript"} {
		if w := serve(handler, "GET", p, "", auth); w.Code != 200 {
			t.Errorf("read peer GET %s = %d, want 200", p, w.Code)
		}
	}
	if got := dev.last(); got == nil || got.Header.Get("Authorization") != "" {
		t.Error("the device token must not reach the device")
	}
	// Read access: no writes.
	if w := serve(handler, "POST", d+"/pty/start", `{}`, auth); w.Code != http.StatusForbidden {
		t.Errorf("read peer start = %d, want 403", w.Code)
	}

	if err := h.st.setPeerAccess(ctx, caller.ID, PeerControl); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"POST", d + "/pty/start"}, {"DELETE", d + "/pty/abc"}, {"POST", d + "/api/sessions/abc/title"}, {"POST", d + "/api/restart"}} {
		if w := serve(handler, c[0], c[1], `{}`, auth); w.Code != 200 {
			t.Errorf("control peer %s %s = %d, want 200", c[0], c[1], w.Code)
		}
	}
	// Never: device management, approvals, typing, the raw terminal,
	// settings, other hub routes.
	for _, c := range [][2]string{
		{"POST", "/api/devices"}, {"POST", "/api/devices/" + caller.ID + "/peer"}, {"DELETE", "/api/devices/" + target.ID},
		{"POST", d + "/approvals/1/decide"}, {"POST", d + "/pty/abc/input"}, {"GET", d + "/pty/abc/stream"},
		{"GET", d + "/ws/pty/abc"}, {"POST", d + "/api/settings"}, {"GET", d + "/api/settings"}, {"POST", d + "/api/sessions/abc/wait"},
		{"GET", "/api/board"}, {"GET", "/api/prefs"}, {"POST", "/api/push/test"}, {"GET", d + "/hub/file"},
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
