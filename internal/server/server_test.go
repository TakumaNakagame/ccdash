package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/model"
)

// newTestServer opens a fresh temp SQLite DB, points $XDG_STATE_HOME at a
// temp dir (so auth.LoadOrCreate doesn't touch the real user's token file),
// and returns a Server plus its auth token for use in test requests.
func newTestServer(t *testing.T) (*Server, *db.DB, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	d, err := db.Open(filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s := New(d, "127.0.0.1:0")
	if s.token == "" {
		t.Fatal("expected a non-empty auth token")
	}
	return s, d, s.token
}

// recorded is the bit of *httptest.ResponseRecorder our assertions need,
// captured after ServeHTTP returns so callers don't have to call Result()
// or Body.Bytes() everywhere.
type recorded struct {
	status int
	body   []byte
}

// do sends a request through the server's real handler chain (mux + rate
// limit + token middleware) in-process, with the given token header (empty
// string omits the header entirely).
func do(t *testing.T, s *Server, method, path, token string, body []byte) recorded {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-Ccdash-Token", token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return recorded{status: rec.Code, body: rec.Body.Bytes()}
}

func insertSession(t *testing.T, d *db.DB, id, transcriptPath string) {
	t.Helper()
	if err := d.UpsertSession(context.Background(), &model.Session{
		SessionID:      id,
		Cwd:            "/tmp/proj",
		TranscriptPath: transcriptPath,
		Status:         model.StatusIdle,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAPISessionsAndApprovals(t *testing.T) {
	s, d, tok := newTestServer(t)
	insertSession(t, d, "sess-1", "")

	// Auth failure: missing token.
	if rec := do(t, s, http.MethodGet, "/api/sessions", "", nil); rec.status != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", rec.status)
	}
	// Auth failure: wrong token.
	if rec := do(t, s, http.MethodGet, "/api/sessions", "wrong-token", nil); rec.status != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", rec.status)
	}

	rec := do(t, s, http.MethodGet, "/api/sessions?archived=0", tok, nil)
	if rec.status != http.StatusOK {
		t.Fatalf("list sessions: status = %d body=%s", rec.status, rec.body)
	}
	var ss []model.Session
	if err := json.Unmarshal(rec.body, &ss); err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].SessionID != "sess-1" {
		t.Fatalf("sessions = %+v", ss)
	}

	rec = do(t, s, http.MethodGet, "/api/approvals", tok, nil)
	if rec.status != http.StatusOK {
		t.Fatalf("list approvals: status = %d body=%s", rec.status, rec.body)
	}
	var as []model.Approval
	if err := json.Unmarshal(rec.body, &as); err != nil {
		t.Fatal(err)
	}
	if len(as) != 0 {
		t.Fatalf("expected no pending approvals, got %+v", as)
	}
}

func TestAPISessionMutations(t *testing.T) {
	s, d, tok := newTestServer(t)
	insertSession(t, d, "sess-1", "")

	cases := []struct {
		path string
		body string
	}{
		{"/api/sessions/sess-1/archive", `{"archived":true}`},
		{"/api/sessions/sess-1/favorite", `{"favorite":true}`},
		{"/api/sessions/sess-1/title", `{"title":"custom title"}`},
		{"/api/sessions/sess-1/group", `{"group":"my-group"}`},
	}
	for _, c := range cases {
		rec := do(t, s, http.MethodPost, c.path, tok, []byte(c.body))
		if rec.status != http.StatusOK {
			t.Fatalf("%s: status = %d body=%s", c.path, rec.status, rec.body)
		}
	}

	ss, err := d.ListSessions(context.Background(), true) // archived view
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 {
		t.Fatalf("expected the archived session to show up in the archive view, got %+v", ss)
	}
	got := ss[0]
	if !got.Favorite || got.CustomTitle != "custom title" || got.UserGroup != "my-group" {
		t.Fatalf("session after mutations = %+v", got)
	}

	// Bad body → 400.
	rec := do(t, s, http.MethodPost, "/api/sessions/sess-1/archive", tok, []byte("not json"))
	if rec.status != http.StatusBadRequest {
		t.Fatalf("bad body: status = %d, want 400", rec.status)
	}
}

func TestAPISettings(t *testing.T) {
	s, _, tok := newTestServer(t)

	rec := do(t, s, http.MethodGet, "/api/settings", tok, nil)
	if rec.status != http.StatusOK {
		t.Fatalf("get settings: status = %d body=%s", rec.status, rec.body)
	}
	var all map[string]string
	if err := json.Unmarshal(rec.body, &all); err != nil {
		t.Fatal(err)
	}
	if _, ok := all["approve_enabled"]; !ok {
		t.Fatalf("expected approve_enabled key in settings map, got %+v", all)
	}

	rec = do(t, s, http.MethodPut, "/api/settings/summary_enabled", tok, []byte(`{"value":"0"}`))
	if rec.status != http.StatusOK {
		t.Fatalf("put setting: status = %d body=%s", rec.status, rec.body)
	}
	rec = do(t, s, http.MethodGet, "/api/settings", tok, nil)
	_ = json.Unmarshal(rec.body, &all)
	if all["summary_enabled"] != "0" {
		t.Fatalf("summary_enabled = %q, want %q", all["summary_enabled"], "0")
	}
}

func TestAPISummarizeGatingAndNotFound(t *testing.T) {
	s, d, tok := newTestServer(t)

	// Unknown session id → 404, never touches the filesystem.
	rec := do(t, s, http.MethodPost, "/api/sessions/does-not-exist/summarize", tok, nil)
	if rec.status != http.StatusNotFound {
		t.Fatalf("unknown session: status = %d, want 404", rec.status)
	}

	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "sess-2.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	insertSession(t, d, "sess-2", transcriptPath)

	// summary_enabled off → 403, and it must not have flipped summary_status.
	if err := d.SetSetting(context.Background(), "summary_enabled", "0"); err != nil {
		t.Fatal(err)
	}
	rec = do(t, s, http.MethodPost, "/api/sessions/sess-2/summarize", tok, nil)
	if rec.status != http.StatusForbidden {
		t.Fatalf("summary disabled: status = %d, want 403", rec.status)
	}

	// summary_enabled on → 202 Accepted (the actual claude -p run happens
	// async in a goroutine and isn't awaited here).
	if err := d.SetSetting(context.Background(), "summary_enabled", "1"); err != nil {
		t.Fatal(err)
	}
	rec = do(t, s, http.MethodPost, "/api/sessions/sess-2/summarize", tok, nil)
	if rec.status != http.StatusAccepted {
		t.Fatalf("summarize: status = %d body=%s", rec.status, rec.body)
	}
}

func TestAPITranscript(t *testing.T) {
	s, d, tok := newTestServer(t)

	// Unknown session id → 404. Path safety: there is no client-suppliable
	// path anywhere in this request — the id must resolve through the DB.
	rec := do(t, s, http.MethodGet, "/api/sessions/does-not-exist/transcript", tok, nil)
	if rec.status != http.StatusNotFound {
		t.Fatalf("unknown session transcript: status = %d, want 404", rec.status)
	}

	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "sess-3.jsonl")
	const line = `{"type":"user","message":{"role":"user","content":"hello there"}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(strings.Repeat(line, 3)), 0o600); err != nil {
		t.Fatal(err)
	}
	insertSession(t, d, "sess-3", transcriptPath)

	rec = do(t, s, http.MethodGet, "/api/sessions/sess-3/transcript?mode=stat", tok, nil)
	if rec.status != http.StatusOK {
		t.Fatalf("stat: status = %d body=%s", rec.status, rec.body)
	}
	var env struct {
		Mtime string `json:"mtime"`
		Size  int64  `json:"size"`
		Data  string `json:"data"`
	}
	if err := json.Unmarshal(rec.body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Mtime == "" || env.Size == 0 || env.Data != "" {
		t.Fatalf("stat envelope = %+v", env)
	}

	rec = do(t, s, http.MethodGet, "/api/sessions/sess-3/transcript?mode=full", tok, nil)
	if rec.status != http.StatusOK {
		t.Fatalf("full: status = %d body=%s", rec.status, rec.body)
	}
	if err := json.Unmarshal(rec.body, &env); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != strings.Repeat(line, 3) {
		t.Fatalf("full transcript bytes mismatch: got %q", data)
	}

	rec = do(t, s, http.MethodGet, "/api/sessions/sess-3/transcript?mode=tail&bytes=10", tok, nil)
	if rec.status != http.StatusOK {
		t.Fatalf("tail: status = %d body=%s", rec.status, rec.body)
	}
	// Fresh struct: mode=tail's envelope omits "data" entirely when the
	// trimmed tail is empty (omitempty on an empty string), and decoding
	// into the same `env` used above would silently keep the previous
	// mode=full payload instead of catching that.
	var tailEnv struct {
		Mtime string `json:"mtime"`
		Size  int64  `json:"size"`
		Data  string `json:"data"`
	}
	if err := json.Unmarshal(rec.body, &tailEnv); err != nil {
		t.Fatal(err)
	}
	tailData, err := base64.StdEncoding.DecodeString(tailEnv.Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(tailData) >= len(strings.Repeat(line, 3)) {
		t.Fatalf("expected a tail read to be smaller than the full file, got %d bytes", len(tailData))
	}
}
