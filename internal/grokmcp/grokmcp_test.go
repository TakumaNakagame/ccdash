package grokmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/store"
)

// TestMain keeps the tests off the real state dir (a joined machine's
// hub.json would give the rows a letter).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ccdash-grok-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fake is the local store with the collector's PTY calls recorded instead
// of sent; a start makes the new session's row the way the collector would.
type fake struct {
	store.Store
	d      *db.DB
	starts []store.StartRequest
	inputs map[string][]store.Input
	hosted []string
	newID  string
}

func (f *fake) StartSession(ctx context.Context, req store.StartRequest) (string, error) {
	f.starts = append(f.starts, req)
	if req.ResumeID == "" && f.newID != "" {
		s := model.Session{SessionID: f.newID, Cwd: req.Cwd, Title: req.Prompt, Status: model.StatusActive}
		if err := f.d.UpsertSession(ctx, &s); err != nil {
			return "", err
		}
		if err := f.d.SetOrigin(ctx, f.newID, req.Origin); err != nil {
			return "", err
		}
	}
	return "pid-1", nil
}

func (f *fake) SendInput(ctx context.Context, key string, in store.Input) error {
	f.inputs[key] = append(f.inputs[key], in)
	return nil
}

func (f *fake) HostedSessions(context.Context) ([]string, error) { return f.hosted, nil }

func (f *fake) WaitingSpawns(context.Context) ([]store.WaitingSpawn, error) { return nil, nil }

func setup(t *testing.T) (*Server, *fake) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	tp := filepath.Join(dir, "g.jsonl")
	_ = os.WriteFile(tp, []byte(
		`{"type":"user","timestamp":"2026-10-23T10:00:00Z","message":{"role":"user","content":"look into the DNS alert"}}`+"\n"+
			`{"type":"assistant","timestamp":"2026-10-23T10:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"Opened PR #12"},{"type":"tool_use","name":"Bash","input":{"command":"cat /secret"}}]}}`+"\n"), 0o600)
	for _, s := range []model.Session{
		{SessionID: "own", Cwd: "/home/u/inbox", Title: "dns alert", TranscriptPath: tp, Status: model.StatusIdle},
		{SessionID: "ops", Cwd: "/home/u/lab", Title: "operator's work", TranscriptPath: tp, Status: model.StatusIdle},
	} {
		if err := d.UpsertSession(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SetOrigin(ctx, "own", Origin); err != nil {
		t.Fatal(err)
	}
	_ = d.SetWait(ctx, "ops", model.WaitReport, "PR #3 merged\nall good")
	f := &fake{Store: store.NewLocal(d), d: d, inputs: map[string][]store.Input{}, hosted: []string{"own", "ops"}}
	s := New(f, Config{Cwd: "/home/u/inbox", Project: "Grok"}, "test")
	s.startWait, s.tick = 2*time.Second, 10*time.Millisecond
	return s, f
}

func call(t *testing.T, s *Server, name, args string) (string, error) {
	t.Helper()
	return s.call(context.Background(), name, json.RawMessage(args))
}

func TestListShowsOnlyBriefRows(t *testing.T) {
	s, _ := setup(t)
	out, err := call(t, s, "list_sessions", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"#1"`, `"#2"`, `"PR #3 merged all good"`, `"started_by": "you"`} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %s:\n%s", want, out)
		}
	}
	for _, leak := range []string{"/home/u/lab", `"ops"`, "PR #12"} {
		if strings.Contains(out, leak) {
			t.Errorf("list shows %s:\n%s", leak, out)
		}
	}
	mine, _ := call(t, s, "list_sessions", `{"started_by_me":true}`)
	if strings.Contains(mine, "operator's work") {
		t.Errorf("started_by_me lists the operator's session:\n%s", mine)
	}
}

func TestReadOnlyOwn(t *testing.T) {
	s, _ := setup(t)
	out, err := call(t, s, "read_session", `{"session":"#1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Opened PR #12") || !strings.Contains(out, "look into the DNS alert") {
		t.Errorf("read lacks the conversation:\n%s", out)
	}
	if strings.Contains(out, "/secret") {
		t.Errorf("read shows tool calls:\n%s", out)
	}
	for _, ref := range []string{"#2", "ops", "2"} {
		if _, err := call(t, s, "read_session", `{"session":"`+ref+`"}`); err == nil || !strings.Contains(err.Error(), "not a session you started") {
			t.Errorf("read %s: %v", ref, err)
		}
	}
}

func TestSend(t *testing.T) {
	s, f := setup(t)
	if _, err := call(t, s, "send_message", `{"session":"#1","text":"also check AAAA"}`); err != nil {
		t.Fatal(err)
	}
	if got := f.inputs["own"]; len(got) != 1 || got[0].Text != "also check AAAA" || !got[0].Submit {
		t.Errorf("inputs = %+v", got)
	}
	if _, err := call(t, s, "send_message", `{"session":"#2","text":"hi"}`); err == nil {
		t.Error("sent to the operator's session")
	}
	for _, bad := range []string{"/permissions", "!rm -rf ~", "# remember", "-p x", "a\u001b[Zb", "   "} {
		b, _ := json.Marshal(map[string]string{"session": "#1", "text": bad})
		if _, err := call(t, s, "send_message", string(b)); err == nil {
			t.Errorf("sent %q", bad)
		}
	}
	// A prompt on screen would take the Enter.
	_ = f.d.SetAttention(context.Background(), "own", model.AttentionNeedsYou, "確認: Bash")
	if _, err := call(t, s, "send_message", `{"session":"#1","text":"go on"}`); err == nil {
		t.Error("sent over a dialog")
	}
	if len(f.inputs["own"]) != 1 || len(f.inputs["ops"]) != 0 {
		t.Errorf("inputs = %+v", f.inputs)
	}
}

func TestSendResumesStopped(t *testing.T) {
	s, f := setup(t)
	f.hosted = nil
	ctx := context.Background()
	x := model.Session{SessionID: "own", Cwd: "/home/u/inbox", Status: model.StatusStopped}
	_ = f.d.UpsertSession(ctx, &x)
	if _, err := call(t, s, "send_message", `{"session":"own","text":"any news?"}`); err != nil {
		t.Fatal(err)
	}
	if len(f.starts) != 1 || f.starts[0].ResumeID != "own" || f.starts[0].PermissionMode != "auto" || f.starts[0].Prompt != "any news?" {
		t.Errorf("starts = %+v", f.starts)
	}
}

func TestStart(t *testing.T) {
	s, f := setup(t)
	f.newID = "new"
	out, err := call(t, s, "start_session", `{"request":"調べて PR にして"}`)
	if err != nil {
		t.Fatal(err)
	}
	r := f.starts[0]
	if r.Cwd != "/home/u/inbox" || r.PermissionMode != "auto" || r.Origin != Origin || r.Project != "Grok" || r.Prompt != "調べて PR にして" {
		t.Errorf("start = %+v", r)
	}
	if !strings.Contains(out, `"#3"`) {
		t.Errorf("start didn't return the new ref:\n%s", out)
	}
	if _, err := call(t, s, "start_session", `{"request":"/login"}`); err == nil {
		t.Error("started a slash command")
	}
	if _, err := call(t, s, "start_session", `{"request":"x","cwd":"/etc"}`); err == nil {
		t.Error("took a cwd argument")
	}
}

func TestHTTP(t *testing.T) {
	s, _ := setup(t)
	token := strings.Repeat("k", 40)
	h := NewHandler(s, token, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	post := func(auth, remote, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
		r.RemoteAddr = remote
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if w := post("", "127.0.0.1:5", list); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
	if w := post("wrong", "127.0.0.1:5", list); w.Code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", w.Code)
	}
	if w := post(token, "10.0.0.9:5", list); w.Code != http.StatusForbidden {
		t.Errorf("outside --allow: %d", w.Code)
	}
	w := post(token, "127.0.0.1:5", list)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "send_message") {
		t.Errorf("tools/list: %d %s", w.Code, w.Body.String())
	}
	if w := post(token, "127.0.0.1:5", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); w.Code != http.StatusAccepted {
		t.Errorf("notification: %d", w.Code)
	}
	w = post(token, "127.0.0.1:5", `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	if !strings.Contains(w.Body.String(), `"2025-11-25"`) {
		t.Errorf("initialize: %s", w.Body.String())
	}
}
