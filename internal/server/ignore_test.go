package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/settings"
)

// ignoreHookReq sends one hook event with the given extra headers.
func ignoreHookReq(t *testing.T, s *Server, tok, path, id, cwd string, hdr map[string]string) {
	t.Helper()
	body := `{"session_id":"` + id + `","cwd":"` + cwd + `","tool_name":"Bash","tool_input":{"command":"ls"}}`
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("X-Ccdash-Token", tok)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
}

func fullTurn(t *testing.T, s *Server, tok, id, cwd string, hdr map[string]string) {
	t.Helper()
	for _, p := range []string{"/hooks/session-start", "/hooks/user-prompt", "/hooks/pre-tool", "/hooks/permission-request", "/hooks/post-tool", "/hooks/stop", "/hooks/session-end"} {
		ignoreHookReq(t, s, tok, p, id, cwd, hdr)
	}
}

// TestIgnoreMarker: a run with CCDASH_IGNORE leaves no row, no approval
// (its PermissionRequest is answered at once with no decision) and is
// remembered, so later hooks without the header and discovery skip it.
func TestIgnoreMarker(t *testing.T) {
	s, d, tok := newTestServer(t)
	ctx := context.Background()
	start := time.Now()
	fullTurn(t, s, tok, "cron", "/home/u/nudge", map[string]string{"X-Ccdash-Ignore": "1", "X-Ccdash-Entrypoint": "sdk-cli"})
	if time.Since(start) > 5*time.Second {
		t.Fatal("the ignored PermissionRequest blocked")
	}
	if d.SessionExists(ctx, "cron") {
		t.Fatal("ignored run got a row")
	}
	if ap, _ := d.ListPendingApprovals(ctx); len(ap) != 0 {
		t.Fatalf("ignored run left approvals: %+v", ap)
	}
	if !d.IsIgnored(ctx, "cron") {
		t.Fatal("not remembered")
	}
	ignoreHookReq(t, s, tok, "/hooks/user-prompt", "cron", "/home/u/nudge", nil)
	if d.SessionExists(ctx, "cron") {
		t.Fatal("a later hook without the header brought it back")
	}
	if !s.discoverySkip(ctx, map[string]bool{"cron": true}, "", "cron", "/home/u/nudge", "sdk-cli") {
		t.Fatal("discovery would list it")
	}

	// "0" / empty is not a request to be ignored.
	ignoreHookReq(t, s, tok, "/hooks/session-start", "plain", "/home/u/x", map[string]string{"X-Ccdash-Ignore": "0"})
	if !d.SessionExists(ctx, "plain") {
		t.Fatal(`CCDASH_IGNORE=0 hid the session`)
	}
}

// TestIgnoreNeverHidesHostedOrKept: a session ccdash started (PTY hook tag)
// or a row the operator kept (pinned) shows even with the marker.
func TestIgnoreNeverHidesHostedOrKept(t *testing.T) {
	s, d, tok := newTestServer(t)
	ctx := context.Background()
	tag := strconv.Itoa(ptyTagBase + 7)
	ignoreHookReq(t, s, tok, "/hooks/session-start", "hosted", "/home/u/nudge", map[string]string{"X-Ccdash-Ignore": "1", "X-Ccdash-Wrapper-Pid": tag})
	if !d.SessionExists(ctx, "hosted") || d.IsIgnored(ctx, "hosted") {
		t.Fatal("a ccdash-hosted session was hidden")
	}

	ignoreHookReq(t, s, tok, "/hooks/session-start", "mine", "/home/u/p", nil)
	if err := d.SetFavorite(ctx, "mine", true); err != nil {
		t.Fatal(err)
	}
	ignoreHookReq(t, s, tok, "/hooks/user-prompt", "mine", "/home/u/p", map[string]string{"X-Ccdash-Ignore": "1"})
	if !d.SessionExists(ctx, "mine") || d.IsIgnored(ctx, "mine") {
		t.Fatal("a pinned session was forgotten")
	}
}

// TestIgnoreDirs: headless runs under ignore_dirs are dropped; interactive
// ones there, and headless ones elsewhere, still show.
func TestIgnoreDirs(t *testing.T) {
	s, d, tok := newTestServer(t)
	ctx := context.Background()
	cfg, _ := settings.Load(ctx, d)
	if _, err := settings.Set(ctx, d, cfg, "ignore_dirs", "/home/u/nudge"); err != nil {
		t.Fatal(err)
	}
	headless := map[string]string{"X-Ccdash-Entrypoint": "sdk-cli"}
	fullTurn(t, s, tok, "slot", "/home/u/nudge/sub", headless)
	ignoreHookReq(t, s, tok, "/hooks/session-start", "chat", "/home/u/nudge", map[string]string{"X-Ccdash-Entrypoint": "cli"})
	ignoreHookReq(t, s, tok, "/hooks/session-start", "other", "/home/u/nudgeX", headless)
	if d.SessionExists(ctx, "slot") {
		t.Error("headless run under ignore_dirs got a row")
	}
	if !d.SessionExists(ctx, "chat") {
		t.Error("interactive session under ignore_dirs was hidden")
	}
	if !d.SessionExists(ctx, "other") {
		t.Error("a sibling folder matched ignore_dirs")
	}

	// Discovery: a new headless transcript there is skipped (and
	// remembered); an existing row or an interactive one is kept.
	ignored := map[string]bool{}
	if !s.discoverySkip(ctx, ignored, "/home/u/nudge", "late", "/home/u/nudge", "sdk-cli") || !d.IsIgnored(ctx, "late") {
		t.Error("discovery would list a headless run under ignore_dirs")
	}
	if s.discoverySkip(ctx, ignored, "/home/u/nudge", "chat", "/home/u/nudge", "sdk-cli") {
		t.Error("discovery dropped an existing row")
	}
	if s.discoverySkip(ctx, ignored, "/home/u/nudge", "new-chat", "/home/u/nudge", "cli") {
		t.Error("discovery dropped an interactive session")
	}
}

// TestSessionEndUnknown: `claude mcp list` fires only SessionEnd and has no
// transcript — no row for it. A known session's SessionEnd still stops it.
func TestSessionEndUnknown(t *testing.T) {
	s, d, tok := newTestServer(t)
	ctx := context.Background()
	ignoreHookReq(t, s, tok, "/hooks/session-end", "mcp-list", "/home/u", map[string]string{"X-Ccdash-Entrypoint": "sdk-cli"})
	if d.SessionExists(ctx, "mcp-list") {
		t.Fatal("a SessionEnd-only run got a row")
	}
	ignoreHookReq(t, s, tok, "/hooks/session-start", "real", "/home/u/p", nil)
	ignoreHookReq(t, s, tok, "/hooks/session-end", "real", "/home/u/p", nil)
	got, ok, _ := d.GetSession(ctx, "real")
	if !ok || got.Status != "stopped" {
		t.Fatalf("real session: ok=%v status=%q", ok, got.Status)
	}
}
