package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// fakeLogin stands in for `claude auth login`: it prints a sign-in URL the
// way Claude Code does, reads the code from stdin and writes it to a file
// (so the test sees it reached the process), then exits with exitCode.
func fakeLogin(t *testing.T, exitCode int) string {
	t.Helper()
	got := filepath.Join(t.TempDir(), "code")
	script := `echo "Opening browser to sign in…"; echo "If the browser didn't open, visit:"; ` +
		`echo "https://claude.ai/oauth/authorize?abcDEF123"; printf "Paste code here if prompted > "; ` +
		`read c; printf %s "$c" > ` + got + `; exit ` + string(rune('0'+exitCode))
	old := newAuthCmd
	newAuthCmd = func(args ...string) *exec.Cmd { return exec.Command("sh", "-c", script) }
	oldStatus := runAuthStatus
	runAuthStatus = func(context.Context) (authStatus, error) {
		return authStatus{LoggedIn: true, LoginExpiresAt: time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339Nano), Email: "me@example.com"}, nil
	}
	t.Cleanup(func() { newAuthCmd, runAuthStatus = old, oldStatus })
	return got
}

func TestLoginFlow(t *testing.T) {
	s, d, tok := newTestServer(t)
	s.login.init()
	ctx := context.Background()
	insertSession(t, d, "sess-1", "")
	if _, err := d.MarkLoginExpired(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	var st model.LoginState
	_ = json.Unmarshal(do(t, s, "GET", "/api/login", tok, nil).body, &st)
	if !st.Expired || len(st.Sessions) != 1 || st.Sessions[0].SessionID != "sess-1" {
		t.Fatalf("state = %+v, want expired with sess-1", st)
	}

	codeFile := fakeLogin(t, 0)
	r := do(t, s, "POST", "/api/login/start", tok, []byte(`{}`))
	var f model.LoginFlow
	_ = json.Unmarshal(r.body, &f)
	if r.status != 200 || f.State != model.LoginURL || f.URL != "https://claude.ai/oauth/authorize?abcDEF123" {
		t.Fatalf("start = %d %s", r.status, r.body)
	}
	// The state everyone reads (the hub's watcher, /api/devices) has no URL.
	if b := do(t, s, "GET", "/api/login", tok, nil).body; strings.Contains(string(b), "https://") {
		t.Fatalf("GET /api/login carries the URL: %s", b)
	}
	if b := do(t, s, "GET", "/api/login/flow", tok, nil).body; !strings.Contains(string(b), "abcDEF123") {
		t.Fatalf("GET /api/login/flow = %s, want the URL", b)
	}
	// Garbage never reaches the process.
	if r := do(t, s, "POST", "/api/login/code", tok, []byte(`{"id":"`+f.ID+`","code":"x; rm -rf /"}`)); r.status != 400 {
		t.Fatalf("bad code = %d", r.status)
	}
	if r := do(t, s, "POST", "/api/login/code", tok, []byte(`{"id":"other","code":"abcdefgh12345#state"}`)); r.status != 409 {
		t.Fatalf("code for another flow = %d", r.status)
	}
	r = do(t, s, "POST", "/api/login/code", tok, []byte(`{"id":"`+f.ID+`","code":" abcdefgh12345#state "}`))
	f = model.LoginFlow{}
	_ = json.Unmarshal(r.body, &f)
	if r.status != 200 || f.State != model.LoginDone || f.Account != "me@example.com" || f.URL != "" {
		t.Fatalf("code = %d %s", r.status, r.body)
	}
	if b, _ := os.ReadFile(codeFile); string(b) != "abcdefgh12345#state" {
		t.Fatalf("process read %q", b)
	}
	// Not hosted: the mark turns into "send the next prompt".
	if len(f.Left) != 1 {
		t.Fatalf("left = %v", f.Left)
	}
	x, _, _ := d.GetSession(ctx, "sess-1")
	if x.AttentionReason != model.LoginDoneReason {
		t.Fatalf("reason = %q", x.AttentionReason)
	}
	_ = json.Unmarshal(do(t, s, "GET", "/api/login", tok, nil).body, &st)
	if st.Expired || st.Flow == nil || st.Flow.State != model.LoginDone {
		t.Fatalf("state after login = %+v", st)
	}
	// An error from before this login is history now.
	s.noteLoginFailure(ctx, "sess-1", time.Now().Add(-time.Minute))
	if ids, _ := d.LoginExpiredIDs(ctx); len(ids) != 0 {
		t.Fatalf("old error re-marked: %v", ids)
	}
}

func TestLoginFlowFails(t *testing.T) {
	s, _, tok := newTestServer(t)
	fakeLogin(t, 1)
	var f model.LoginFlow
	_ = json.Unmarshal(do(t, s, "POST", "/api/login/start", tok, []byte(`{}`)).body, &f)
	r := do(t, s, "POST", "/api/login/code", tok, []byte(`{"id":"`+f.ID+`","code":"abcdefgh12345"}`))
	f = model.LoginFlow{}
	_ = json.Unmarshal(r.body, &f)
	if f.State != model.LoginFailed || strings.Contains(f.Error, "https://") {
		t.Fatalf("flow = %+v", f)
	}
}

func TestLoginCancel(t *testing.T) {
	s, _, tok := newTestServer(t)
	fakeLogin(t, 0)
	do(t, s, "POST", "/api/login/start", tok, []byte(`{}`))
	do(t, s, "POST", "/api/login/cancel", tok, []byte(`{}`))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var st model.LoginState
		_ = json.Unmarshal(do(t, s, "GET", "/api/login", tok, nil).body, &st)
		if st.Flow != nil && st.Flow.State == model.LoginCancelled && !st.Flow.EndedAt.IsZero() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("flow not cancelled")
}

func TestStopFailureMarksLogin(t *testing.T) {
	s, d, tok := newTestServer(t)
	s.login.init()
	insertSession(t, d, "sess-2", "")
	do(t, s, "POST", "/hooks/stop-failure", tok, []byte(`{"session_id":"sess-2","hook_event_name":"StopFailure","error":"authentication_failed"}`))
	x, _, _ := d.GetSession(context.Background(), "sess-2")
	if x.Attention != model.AttentionNeedsYou || x.AttentionReason != model.LoginReason {
		t.Fatalf("attention = %q %q", x.Attention, x.AttentionReason)
	}
	do(t, s, "POST", "/hooks/stop-failure", tok, []byte(`{"session_id":"sess-2","hook_event_name":"StopFailure","error":"rate_limit"}`))
	x, _, _ = d.GetSession(context.Background(), "sess-2")
	if x.Attention != model.AttentionDone {
		t.Fatalf("rate limit attention = %q", x.Attention)
	}
}

func TestAuthStatusSubscription(t *testing.T) {
	for _, c := range []struct {
		st   authStatus
		want bool
	}{
		{authStatus{AuthMethod: "claude.ai", APIProvider: "firstParty"}, true},
		{authStatus{AuthMethod: "none", APIProvider: "firstParty"}, true},
		{authStatus{AuthMethod: "api_key", APIProvider: "firstParty"}, false},
		{authStatus{AuthMethod: "none", APIProvider: "bedrock"}, false},
	} {
		if got := c.st.subscription(); got != c.want {
			t.Errorf("%+v.subscription() = %v, want %v", c.st, got, c.want)
		}
	}
}

func TestParseAuthStatus(t *testing.T) {
	out := "rc noise {\n" + `{"loggedIn": false, "authMethod": "none"}` + "\n"
	st, err := parseAuthStatus(out)
	if err != nil || st.LoggedIn {
		t.Fatalf("st = %+v, %v", st, err)
	}
	st, err = parseAuthStatus(`{"loggedIn":true,"loginExpiresAt":"2026-12-10T19:24:33.124Z","email":"x@y"}`)
	if err != nil || !st.LoggedIn || st.LoginExpiresAt == "" {
		t.Fatalf("st = %+v, %v", st, err)
	}
}
