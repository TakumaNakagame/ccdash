package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/settings"
)

const testToken = "test-token-abc123"

// newTestAPI builds a small in-memory mock of internal/server's remote-mode
// routes, just enough surface for Remote's methods to exercise: auth
// checking, each endpoint shape, and a couple of non-2xx paths.
func newTestAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Ccdash-Token") != testToken {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("GET /api/sessions", authed(func(w http.ResponseWriter, r *http.Request) {
		var ss []model.Session
		if r.URL.Query().Get("archived") == "1" {
			ss = []model.Session{{SessionID: "archived-1", Archived: true}}
		} else {
			ss = []model.Session{{SessionID: "s1", Cwd: "/tmp"}}
		}
		_ = json.NewEncoder(w).Encode(ss)
	}))
	mux.HandleFunc("GET /api/approvals", authed(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]model.Approval{{ID: 7, SessionID: "s1", Tool: "Bash"}})
	}))
	mux.HandleFunc("POST /api/sessions/{id}/archive", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "boom" {
			http.Error(w, "kaboom", http.StatusInternalServerError)
			return
		}
		var body struct {
			Archived bool `json:"archived"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Archived {
			http.Error(w, "expected archived=true", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	mux.HandleFunc("POST /api/sessions/{id}/favorite", authed(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	mux.HandleFunc("POST /api/sessions/{id}/title", authed(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	mux.HandleFunc("POST /api/sessions/{id}/group", authed(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	mux.HandleFunc("POST /api/sessions/{id}/summarize", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "disabled" {
			http.Error(w, "summarize is disabled", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	mux.HandleFunc("GET /api/sessions/{id}/transcript", authed(func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		env := map[string]any{
			"mtime": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Format(time.RFC3339Nano),
			"size":  int64(42),
		}
		if mode != "stat" {
			data := []byte(`{"type":"user","message":{"role":"user","content":"hello"}}` + "\n")
			env["data"] = base64.StdEncoding.EncodeToString(data)
		}
		_ = json.NewEncoder(w).Encode(env)
	}))
	mux.HandleFunc("GET /api/settings", authed(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"approve_enabled": "1", "summary_enabled": "0"})
	}))
	mux.HandleFunc("PUT /api/settings/{key}", authed(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	mux.HandleFunc("POST /approvals/{id}/decide", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "999" {
			http.Error(w, "no pending hold for that id", http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	return httptest.NewServer(mux)
}

func TestRemoteListSessions(t *testing.T) {
	srv := newTestAPI(t)
	defer srv.Close()
	r := NewRemote(srv.URL, testToken)

	ss, err := r.ListSessions(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].SessionID != "s1" {
		t.Fatalf("ListSessions(false) = %+v", ss)
	}

	ss, err = r.ListSessions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].SessionID != "archived-1" {
		t.Fatalf("ListSessions(true) = %+v", ss)
	}
}

func TestRemoteListPendingApprovals(t *testing.T) {
	srv := newTestAPI(t)
	defer srv.Close()
	r := NewRemote(srv.URL, testToken)

	as, err := r.ListPendingApprovals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 1 || as[0].ID != 7 {
		t.Fatalf("ListPendingApprovals = %+v", as)
	}
}

func TestRemoteSessionMutations(t *testing.T) {
	srv := newTestAPI(t)
	defer srv.Close()
	r := NewRemote(srv.URL, testToken)
	ctx := context.Background()

	if err := r.SetArchived(ctx, "s1", true); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}
	if err := r.SetFavorite(ctx, "s1", true); err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}
	if err := r.SetCustomTitle(ctx, "s1", "hello"); err != nil {
		t.Fatalf("SetCustomTitle: %v", err)
	}
	if err := r.SetUserGroup(ctx, "s1", "grp"); err != nil {
		t.Fatalf("SetUserGroup: %v", err)
	}
}

func TestRemoteDecideApproval(t *testing.T) {
	srv := newTestAPI(t)
	defer srv.Close()
	r := NewRemote(srv.URL, testToken)
	ctx := context.Background()

	if err := r.DecideApproval(ctx, 1, "allow", "", false); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	// Non-2xx path: the mock server 410s id 999 to simulate an already
	// resolved / timed out approval.
	if err := r.DecideApproval(ctx, 999, "deny", "", false); err == nil {
		t.Fatal("expected an error for an already-resolved approval id")
	}
}

func TestRemoteSettings(t *testing.T) {
	srv := newTestAPI(t)
	defer srv.Close()
	r := NewRemote(srv.URL, testToken)
	ctx := context.Background()

	v, err := r.GetSetting(ctx, "approve_enabled")
	if err != nil {
		t.Fatal(err)
	}
	if v != "1" {
		t.Fatalf("GetSetting(approve_enabled) = %q, want 1", v)
	}
	v, err = r.GetSetting(ctx, "summary_enabled")
	if err != nil {
		t.Fatal(err)
	}
	if v != "0" {
		t.Fatalf("GetSetting(summary_enabled) = %q, want 0", v)
	}
	// Unknown key: not present in the map, GetSetting should just return "".
	v, err = r.GetSetting(ctx, "does_not_exist")
	if err != nil {
		t.Fatal(err)
	}
	if v != "" {
		t.Fatalf("GetSetting(does_not_exist) = %q, want empty", v)
	}
	if err := r.SetSetting(ctx, "tail_budget_kb", "512"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
}

func TestRemoteSummarize(t *testing.T) {
	srv := newTestAPI(t)
	defer srv.Close()
	r := NewRemote(srv.URL, testToken)
	ctx := context.Background()

	if err := r.Summarize(ctx, "s1"); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	// Non-2xx path: server 403s a session named "disabled" to simulate the
	// summary_enabled=off gate.
	if err := r.Summarize(ctx, "disabled"); err == nil {
		t.Fatal("expected an error when summarize is disabled server-side")
	}
}

func TestRemoteTranscript(t *testing.T) {
	srv := newTestAPI(t)
	defer srv.Close()
	r := NewRemote(srv.URL, testToken)
	ctx := context.Background()
	sess := model.Session{SessionID: "s1"}

	mtime, size, err := r.TranscriptStat(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	if size != 42 || mtime.IsZero() {
		t.Fatalf("TranscriptStat = mtime=%v size=%d", mtime, size)
	}

	tail, err := r.TranscriptTail(ctx, sess, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail.Messages) != 1 || tail.Messages[0].Text != "hello" {
		t.Fatalf("TranscriptTail messages = %+v", tail.Messages)
	}
	if tail.Size != 42 {
		t.Fatalf("TranscriptTail size = %d, want 42", tail.Size)
	}

	msgs, err := r.TranscriptFull(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hello" {
		t.Fatalf("TranscriptFull messages = %+v", msgs)
	}
}

func TestRemoteAuthFailure(t *testing.T) {
	srv := newTestAPI(t)
	defer srv.Close()
	r := NewRemote(srv.URL, "wrong-token")

	if _, err := r.ListSessions(context.Background(), false); err == nil {
		t.Fatal("expected an auth error with the wrong token")
	}
	if err := r.SetArchived(context.Background(), "s1", true); err == nil {
		t.Fatal("expected an auth error with the wrong token")
	}
}

func TestRemoteNon2xxIncludesBody(t *testing.T) {
	srv := newTestAPI(t)
	defer srv.Close()
	r := NewRemote(srv.URL, testToken)

	err := r.SetArchived(context.Background(), "boom", true)
	if err == nil {
		t.Fatal("expected an error for session id 'boom'")
	}
	if got := err.Error(); !strings.Contains(got, "kaboom") {
		t.Fatalf("error %q does not surface the server's response body", got)
	}
}

func TestRemoteAllSettings(t *testing.T) {
	srv := newTestAPI(t)
	defer srv.Close()
	r := NewRemote(srv.URL, testToken)

	all, err := r.AllSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if all["approve_enabled"] != "1" || all["summary_enabled"] != "0" {
		t.Fatalf("AllSettings = %+v", all)
	}
}

// TestRemoteSettingsLoadIsOneRoundTrip pins the startup cost of remote
// mode's settings read: settings.Load must issue exactly ONE HTTP request
// (GET /api/settings via AllSettings) — the per-key GetSetting pattern it
// replaced cost ~16 sequential round trips.
func TestRemoteSettingsLoadIsOneRoundTrip(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// layout_mode present → the legacy-layout migration branch (extra
		// GetSetting reads) stays dormant.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"layout_mode":     "auto",
			"approve_enabled": "0",
			"tail_budget_kb":  "512",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := NewRemote(srv.URL, testToken)
	s, err := settings.Load(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if s.ApproveEnabled {
		t.Error("ApproveEnabled should be false")
	}
	if s.TailBudgetKB != 512 {
		t.Errorf("TailBudgetKB = %d, want 512", s.TailBudgetKB)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("settings.Load made %d requests, want exactly 1", n)
	}
}
