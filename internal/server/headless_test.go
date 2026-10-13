package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHeadlessHook: hooks from a `claude -p` run (hook.sh forwards
// CLAUDE_CODE_ENTRYPOINT) file the session away and its Stop leaves no
// "done" attention; an interactive one is untouched.
func TestHeadlessHook(t *testing.T) {
	s, d, tok := newTestServer(t)
	hook := func(path, id, entrypoint string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"session_id":"`+id+`","cwd":"/tmp/proj"}`))
		req.Header.Set("X-Ccdash-Token", tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Ccdash-Entrypoint", entrypoint)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	for _, id := range []string{"cron", "me"} {
		ep := map[string]string{"cron": "sdk-cli", "me": "cli"}[id]
		hook("/hooks/session-start", id, ep)
		hook("/hooks/user-prompt", id, ep)
		hook("/hooks/stop", id, ep)
	}
	cron, _, _ := d.GetSession(context.Background(), "cron")
	if !cron.Archived || cron.Attention != "" {
		t.Errorf("headless: archived=%v attention=%q, want archived and no attention", cron.Archived, cron.Attention)
	}
	me, _, _ := d.GetSession(context.Background(), "me")
	if me.Archived || me.Attention == "" {
		t.Errorf("interactive: archived=%v attention=%q, want shown with attention", me.Archived, me.Attention)
	}
}
