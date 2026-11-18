package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/settings"
	"github.com/TakumaNakagame/ccdash/internal/tunnel"
)

// roleChild swaps the PTY child for `cat` and keeps each start's claude
// arguments and the command itself (its Env is set by spawnPTY).
type roleChild struct {
	mu   sync.Mutex
	args [][]string
	cmds []*exec.Cmd
}

func newRoleChild(t *testing.T) *roleChild {
	t.Helper()
	rc := &roleChild{}
	old := newPTYCommand
	newPTYCommand = func(args ...string) *exec.Cmd {
		c := exec.Command("sh", "-c", "cat")
		rc.mu.Lock()
		rc.args, rc.cmds = append(rc.args, args), append(rc.cmds, c)
		rc.mu.Unlock()
		return c
	}
	t.Cleanup(func() { newPTYCommand = old })
	return rc
}

func (rc *roleChild) last() ([]string, map[string]string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	env := map[string]string{}
	for _, kv := range rc.cmds[len(rc.cmds)-1].Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return rc.args[len(rc.args)-1], env
}

func startBody(t *testing.T, s *Server, tok, body string) (int, string, int) {
	t.Helper()
	r := do(t, s, "POST", "/pty/start", tok, []byte(body))
	var st struct {
		PtyKey string
		Tag    int
	}
	_ = json.Unmarshal(r.body, &st)
	if st.PtyKey != "" {
		t.Cleanup(func() { do(t, s, "DELETE", "/pty/"+st.PtyKey, tok, nil) })
	}
	return r.status, st.PtyKey, st.Tag
}

// firstHook plays the new claude's first hook, which names its session.
func firstHook(t *testing.T, s *Server, tok, sid string, tag int) {
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

const denyArg = "--disallowedTools=Bash,Edit,Write,NotebookEdit,Agent,WebSearch,WebFetch"

// TestStartWithRole: a person's start with a role takes the tools away,
// passes the role's model / effort, tells the child its role in the
// environment and writes the role to the row; nobody else can ask for one.
func TestStartWithRole(t *testing.T) {
	rc := newRoleChild(t)
	s, d, tok := newTestServer(t)
	ctx := context.Background()
	t.Setenv("CCDASH_ROLE", "orchestrator") // an inherited role must never reach a child
	t.Setenv("CCDASH_ROLE_PROJECT", "leak")

	// An ordinary start: nothing of the role, not even an inherited one.
	if code, _, _ := startBody(t, s, tok, `{"cwd":"/tmp","prompt":"hello"}`); code != 200 {
		t.Fatalf("plain start = %d", code)
	}
	args, env := rc.last()
	if !reflect.DeepEqual(args, []string{"hello"}) || env["CCDASH_ROLE"] != "" || env["CCDASH_ROLE_PROJECT"] != "" {
		t.Fatalf("plain start: args %q, role env %q / %q", args, env["CCDASH_ROLE"], env["CCDASH_ROLE_PROJECT"])
	}

	// The global orchestrator: the deny list as ONE argument (two would
	// swallow the prompt), then the role's effort; no default_model.
	_ = d.SetSetting(ctx, "default_model", "opus")
	code, _, tag := startBody(t, s, tok, `{"cwd":"/tmp","prompt":"you are it","role":"orchestrator"}`)
	if code != 200 {
		t.Fatalf("orchestrator start = %d", code)
	}
	args, env = rc.last()
	if want := []string{denyArg, "--effort", "low", "you are it"}; !reflect.DeepEqual(args, want) {
		t.Errorf("orchestrator args = %q, want %q", args, want)
	}
	if env["CCDASH_ROLE"] != model.RoleOrchestrator || env["CCDASH_ROLE_PROJECT"] != "" {
		t.Errorf("orchestrator env = %q / %q", env["CCDASH_ROLE"], env["CCDASH_ROLE_PROJECT"])
	}
	firstHook(t, s, tok, "orch-1", tag)
	if x, _, _ := d.GetSession(ctx, "orch-1"); x.Role != model.RoleOrchestrator {
		t.Errorf("row role = %q", x.Role)
	}

	// A project's orchestrator: the project's own model / effort win over
	// the settings, the request's over both.
	_ = d.SetSetting(ctx, "orchestrator_model", "sonnet")
	if err := d.SetProjectOrchestratorModel(ctx, "web", "haiku", "medium"); err != nil {
		t.Fatal(err)
	}
	code, _, tag = startBody(t, s, tok, `{"cwd":"/tmp","project":"web","role":"project-orchestrator","effort":"high"}`)
	if code != 200 {
		t.Fatalf("project orchestrator start = %d", code)
	}
	args, env = rc.last()
	if want := []string{denyArg, "--model", "haiku", "--effort", "high"}; !reflect.DeepEqual(args, want) {
		t.Errorf("project orchestrator args = %q, want %q", args, want)
	}
	if env["CCDASH_ROLE"] != model.RoleProjectOrchestrator || env["CCDASH_ROLE_PROJECT"] != "web" {
		t.Errorf("project orchestrator env = %q / %q", env["CCDASH_ROLE"], env["CCDASH_ROLE_PROJECT"])
	}
	firstHook(t, s, tok, "porch-1", tag)
	if x, _, _ := d.GetSession(ctx, "porch-1"); x.Role != model.RoleProjectOrchestrator || x.Project != "web" {
		t.Errorf("row = role %q project %q", x.Role, x.Project)
	}
	ps, _ := d.ListProjects(ctx)
	if len(ps) != 1 || ps[0].Orchestrator != "porch-1" || ps[0].OrchestratorModel != "haiku" || ps[0].OrchestratorEffort != "medium" {
		t.Errorf("projects = %+v", ps)
	}

	// Who may not: a start_session ("orchestrated"), a peer device, a
	// wrong role, a project orchestrator without a project.
	for body, want := range map[string]int{
		`{"cwd":"/tmp","prompt":"x","role":"orchestrator","orchestrated":true}`: http.StatusForbidden,
		`{"cwd":"/tmp","role":"boss"}`:                                          http.StatusBadRequest,
		`{"cwd":"/tmp","role":"project-orchestrator"}`:                          http.StatusBadRequest,
	} {
		if code, _, _ := startBody(t, s, tok, body); code != want {
			t.Errorf("%s = %d, want %d", body, code, want)
		}
	}
	req := httptest.NewRequest("POST", "/pty/start", strings.NewReader(`{"cwd":"/tmp","role":"orchestrator"}`))
	req.Header.Set(tunnel.HeaderPeerID, "dev-A")
	rec := httptest.NewRecorder()
	s.tunnelHandler(false).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a peer's start with a role = %d %s, want 403", rec.Code, rec.Body)
	}
	// …while the portal (no peer header) may.
	req = httptest.NewRequest("POST", "/pty/start", strings.NewReader(`{"cwd":"/tmp","role":"orchestrator"}`))
	rec = httptest.NewRecorder()
	s.tunnelHandler(false).ServeHTTP(rec, req)
	var st struct{ PtyKey string }
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	t.Cleanup(func() { do(t, s, "DELETE", "/pty/"+st.PtyKey, tok, nil) })
	if rec.Code != 200 {
		t.Errorf("the portal's start with a role = %d %s", rec.Code, rec.Body)
	}

	// A deny list someone edited into the table by hand never reaches the
	// command line: the default list applies instead. An empty one takes
	// nothing away.
	_ = d.SetSetting(ctx, "orchestrator_deny", "Bash --dangerously-skip-permissions")
	if code, _, _ := startBody(t, s, tok, `{"cwd":"/tmp","role":"orchestrator"}`); code != 200 {
		t.Fatalf("start with a bad deny list = %d", code)
	}
	if args, _ := rc.last(); !reflect.DeepEqual(args, []string{denyArg, "--model", "sonnet", "--effort", "low"}) {
		t.Errorf("bad deny list: args = %q", args)
	}
	cfg, _ := settings.Load(ctx, d)
	if _, err := settings.Set(ctx, d, cfg, "orchestrator_deny", ""); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := startBody(t, s, tok, `{"cwd":"/tmp","role":"orchestrator"}`); code != 200 {
		t.Fatalf("start with an empty deny list = %d", code)
	}
	if args, _ := rc.last(); !reflect.DeepEqual(args, []string{"--model", "sonnet", "--effort", "low"}) {
		t.Errorf("empty deny list: args = %q", args)
	}
}

// TestResumeKeepsRole: every way ccdash resumes a session whose row has a
// role applies the role again — the tools, the environment and, unlike an
// ordinary resume, the model and effort.
func TestResumeKeepsRole(t *testing.T) {
	rc := newRoleChild(t)
	s, d, tok := newTestServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	for _, id := range []string{"orch-1", "porch-1", "plain-1"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: dir, Status: model.StatusStopped}); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.SetProject(ctx, "porch-1", "web")
	if _, err := d.SetRole(ctx, "orch-1", model.RoleOrchestrator, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetRole(ctx, "porch-1", model.RoleProjectOrchestrator, ""); err != nil {
		t.Fatal(err)
	}
	_ = d.SetSetting(ctx, "default_model", "opus")
	_ = d.SetSetting(ctx, "orchestrator_model", "sonnet")
	_ = d.SetSetting(ctx, "orchestrator_effort", "medium")

	// The portal's resume / resume_session, carrying a role the row lacks.
	host(t, s, tok, "plain-1", dir)
	if args, env := rc.last(); !reflect.DeepEqual(args, []string{"--resume", "plain-1"}) || env["CCDASH_ROLE"] != "" {
		t.Errorf("ordinary resume: args %q, role %q", args, env["CCDASH_ROLE"])
	}
	body := fmt.Sprintf(`{"sessionId":"plain-1b","resumeId":"plain-1","cwd":%q,"role":"orchestrator"}`, dir)
	if code, _, _ := startBody(t, s, tok, body); code != 200 {
		t.Fatalf("resume with a role in the body = %d", code)
	}
	if args, env := rc.last(); !reflect.DeepEqual(args, []string{"--resume", "plain-1"}) || env["CCDASH_ROLE"] != "" {
		t.Errorf("a resume took a role from the request: args %q, role %q", args, env["CCDASH_ROLE"])
	}

	host(t, s, tok, "orch-1", dir)
	args, env := rc.last()
	if want := []string{"--resume", "orch-1", denyArg, "--model", "sonnet", "--effort", "medium"}; !reflect.DeepEqual(args, want) {
		t.Errorf("orchestrator resume args = %q, want %q", args, want)
	}
	if env["CCDASH_ROLE"] != model.RoleOrchestrator {
		t.Errorf("orchestrator resume env = %q", env["CCDASH_ROLE"])
	}

	// POST /pty/{key}/restart — what the portal offers after a role change.
	_ = d.SetSetting(ctx, "orchestrator_deny", "Bash Agent")
	if r := do(t, s, "POST", "/pty/orch-1/restart", tok, nil); r.status != 200 {
		t.Fatalf("restart = %d %s", r.status, r.body)
	}
	if args, _ := rc.last(); !reflect.DeepEqual(args, []string{"--resume", "orch-1", "--disallowedTools=Bash,Agent", "--model", "sonnet", "--effort", "medium"}) {
		t.Errorf("restart args = %q", args)
	}

	// A project's orchestrator, resumed by a message that wakes it.
	_ = d.SetProjectOrchestratorModel(ctx, "web", "", "high")
	if _, _, _, err := s.startPTY(ctx, ptyStartReq{SessionID: "porch-1", ResumeID: "porch-1", Cwd: dir, Prompt: "wake up"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { do(t, s, "DELETE", "/pty/porch-1", tok, nil) })
	args, env = rc.last()
	if want := []string{"--resume", "porch-1", "--disallowedTools=Bash,Agent", "--model", "sonnet", "--effort", "high", "wake up"}; !reflect.DeepEqual(args, want) {
		t.Errorf("project orchestrator resume args = %q, want %q", args, want)
	}
	if env["CCDASH_ROLE"] != model.RoleProjectOrchestrator || env["CCDASH_ROLE_PROJECT"] != "web" {
		t.Errorf("project orchestrator resume env = %q / %q", env["CCDASH_ROLE"], env["CCDASH_ROLE_PROJECT"])
	}

	// A role taken away is gone at the next start.
	if _, err := d.SetRole(ctx, "orch-1", "", ""); err != nil {
		t.Fatal(err)
	}
	if r := do(t, s, "POST", "/pty/orch-1/restart", tok, nil); r.status != 200 {
		t.Fatalf("restart = %d %s", r.status, r.body)
	}
	if args, env := rc.last(); !reflect.DeepEqual(args, []string{"--resume", "orch-1"}) || env["CCDASH_ROLE"] != "" {
		t.Errorf("after the role is gone: args %q, role %q", args, env["CCDASH_ROLE"])
	}
}

// TestRoleRoute: the operator's route gives and takes roles, keeps one
// orchestrator per project, hands a role over with the children, and says
// when a restart is needed.
func TestRoleRoute(t *testing.T) {
	newRoleChild(t)
	s, d, tok := newTestServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	for _, id := range []string{"a", "b", "c", "kid-1", "kid-2"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: dir, Status: model.StatusIdle}); err != nil {
			t.Fatal(err)
		}
	}
	set := func(id, body string) (int, model.RoleChange, string) {
		r := do(t, s, "POST", "/api/sessions/"+id+"/role", tok, []byte(body))
		var ch model.RoleChange
		_ = json.Unmarshal(r.body, &ch)
		return r.status, ch, string(r.body)
	}
	role := func(id string) string {
		x, _, _ := d.GetSession(ctx, id)
		return x.Role
	}

	if code, ch, raw := set("a", `{"role":"orchestrator"}`); code != 200 || ch.Role != model.RoleOrchestrator || ch.RestartNeeded || !strings.Contains(raw, `"restart_needed":false`) {
		t.Fatalf("make orchestrator = %d %s", code, raw)
	}
	// Hosted: the change waits for a restart, and the reply says so.
	host(t, s, tok, "b", dir)
	if code, _, raw := set("b", `{"role":"project-orchestrator"}`); code != http.StatusBadRequest || !strings.Contains(raw, "no project") {
		t.Fatalf("project orchestrator without a project = %d %s", code, raw)
	}
	_ = d.SetProject(ctx, "b", "web")
	_ = d.SetProject(ctx, "c", "web")
	if code, ch, raw := set("b", `{"role":"project-orchestrator"}`); code != 200 || ch.Project != "web" || !ch.RestartNeeded {
		t.Fatalf("project orchestrator = %d %s", code, raw)
	}
	// One per project: c replaces b.
	if code, ch, raw := set("c", `{"role":"project-orchestrator"}`); code != 200 || len(ch.Replaced) != 1 || ch.Replaced[0] != "b" || ch.RestartNeeded {
		t.Fatalf("second project orchestrator = %d %s", code, raw)
	}
	if role("b") != "" || role("c") != model.RoleProjectOrchestrator {
		t.Errorf("after the swap: b %q, c %q", role("b"), role("c"))
	}
	if ps, _ := d.ListProjects(ctx); len(ps) != 1 || ps[0].Orchestrator != "c" {
		t.Errorf("projects = %+v", ps)
	}
	// Leaving the project ends the role.
	_ = d.SetProject(ctx, "c", "")
	if ps, _ := d.ListProjects(ctx); role("c") != "" || ps[0].Orchestrator != "" {
		t.Errorf("after leaving the project: role %q, project orchestrator %q", role("c"), ps[0].Orchestrator)
	}

	// Handover: b takes the global role and a's children in one step.
	_ = d.AdoptParent(ctx, "kid-1", "a", "")
	_ = d.AdoptParent(ctx, "kid-2", "a", "dev-9")
	code, ch, raw := set("b", `{"from":"a"}`)
	if code != 200 || ch.Role != model.RoleOrchestrator || ch.Moved != 2 || len(ch.Replaced) != 1 || ch.Replaced[0] != "a" || !ch.RestartNeeded || ch.RestartFrom {
		t.Fatalf("handover = %d %s", code, raw)
	}
	if role("a") != "" || role("b") != model.RoleOrchestrator {
		t.Errorf("after the handover: a %q, b %q", role("a"), role("b"))
	}
	for _, k := range []string{"kid-1", "kid-2"} {
		if x, _, _ := d.GetSession(ctx, k); x.Parent != "b" || x.ParentHost != "" {
			t.Errorf("%s parent = %q (%q)", k, x.Parent, x.ParentHost)
		}
	}

	for body, want := range map[string]int{
		`{"role":"boss"}`:                    http.StatusBadRequest,
		`{"role":"orchestrator","from":"b"}`: http.StatusBadRequest, // from itself
		`{"from":"c"}`:                       http.StatusBadRequest, // c has no role to take
		`{"from":"nobody"}`:                  http.StatusBadRequest,
		`{"from":"../x"}`:                    http.StatusBadRequest,
	} {
		if code, _, raw := set("b", body); code != want {
			t.Errorf("%s = %d %s, want %d", body, code, raw, want)
		}
	}
	if code, _, _ := set("nobody", `{"role":"orchestrator"}`); code != http.StatusNotFound {
		t.Errorf("unknown session = %d, want 404", code)
	}
	if code, ch, _ := set("b", `{"role":""}`); code != 200 || ch.Role != "" || role("b") != "" {
		t.Errorf("take the role away = %d %+v", code, ch)
	}

	// The project's model for its orchestrator.
	if r := do(t, s, "POST", "/api/projects/orchestrator", tok, []byte(`{"project":"web","model":"haiku","effort":"low"}`)); r.status != 200 {
		t.Errorf("project orchestrator model = %d %s", r.status, r.body)
	}
	for _, bad := range []string{`{"project":"web","model":"--x"}`, `{"project":"web","effort":"huge"}`, `{"project":"","model":"haiku"}`} {
		if r := do(t, s, "POST", "/api/projects/orchestrator", tok, []byte(bad)); r.status != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.status)
		}
	}

	// Through the hub: the portal may (attach on), a peer device never.
	for _, c := range []struct {
		path, peer string
		attach     bool
		want       int
	}{
		{"/api/sessions/a/role", "", true, 200},
		{"/api/sessions/a/role", "dev-A", true, http.StatusForbidden},
		{"/api/sessions/a/role", "", false, http.StatusForbidden},
		{"/api/projects/orchestrator", "dev-A", true, http.StatusForbidden},
	} {
		_ = d.SetSetting(ctx, "attach_enabled", map[bool]string{true: "1", false: "0"}[c.attach])
		req := httptest.NewRequest("POST", c.path, strings.NewReader(`{"role":"orchestrator","project":"web"}`))
		if c.peer != "" {
			req.Header.Set(tunnel.HeaderPeerID, c.peer)
		}
		rec := httptest.NewRecorder()
		s.tunnelHandler(false).ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s peer=%q attach=%v through the hub = %d %s, want %d", c.path, c.peer, c.attach, rec.Code, rec.Body, c.want)
		}
	}
	// The handler itself refuses a peer too (a hub that let it through).
	req := httptest.NewRequest("POST", "/api/sessions/c/role", strings.NewReader(`{"role":"orchestrator"}`))
	req.Header.Set("X-Ccdash-Token", tok)
	req.Header.Set(tunnel.HeaderPeerID, "dev-A")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || role("c") != "" {
		t.Errorf("a peer at the handler = %d, role %q", rec.Code, role("c"))
	}
}

// TestHubAllowedRole pins the allowlist entries of the role routes.
func TestHubAllowedRole(t *testing.T) {
	on := settings.Defaults()
	off := on
	off.AttachEnabled = false
	for _, c := range []struct {
		method, path, peer string
		cfg                settings.Settings
		want               bool
	}{
		{"POST", "/api/sessions/abc/role", "", on, true},
		{"POST", "/api/sessions/abc/role", "", off, false},
		{"POST", "/api/sessions/abc/role", "dev-A", on, false},
		{"GET", "/api/sessions/abc/role", "", on, false},
		{"PUT", "/api/sessions/abc/role", "", on, false},
		{"POST", "/api/sessions/abc/role/x", "", on, false},
		{"POST", "/api/projects/orchestrator", "", on, true},
		{"POST", "/api/projects/orchestrator", "", off, false},
		{"POST", "/api/projects/orchestrator", "dev-A", on, false},
		// The orchestrator's settings stay unwritable, like every setting.
		{"PUT", "/api/settings/orchestrator_prompt", "", on, false},
		{"PUT", "/api/settings/orchestrator_deny", "", on, false},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		if c.peer != "" {
			r.Header.Set(tunnel.HeaderPeerID, c.peer)
		}
		if got, why := hubAllowed(r, c.cfg); got != c.want {
			t.Errorf("%s %s peer=%q attach=%v: allowed=%v (%s), want %v", c.method, c.path, c.peer, c.cfg.AttachEnabled, got, why, c.want)
		}
	}
}

// TestOrchestratorPromptIsLocalOnly: a remote-mode client can't write the
// text every orchestrator is told to follow.
func TestOrchestratorPromptIsLocalOnly(t *testing.T) {
	s, d, tok := newTestServer(t)
	if r := do(t, s, "PUT", "/api/settings/orchestrator_prompt", tok, []byte(`{"value":"do everything yourself"}`)); r.status != http.StatusForbidden {
		t.Errorf("PUT orchestrator_prompt = %d, want 403", r.status)
	}
	if v, _ := d.GetSetting(context.Background(), "orchestrator_prompt"); v != "" {
		t.Errorf("stored %q", v)
	}
}

// TestMessageBetweenOrchestrators: the global orchestrator and a project's
// orchestrator may message each other without being parent and child —
// decided by the roles on the rows (this machine's, or the sending
// device's own answer through the hub), never by the request.
func TestMessageBetweenOrchestrators(t *testing.T) {
	msgFast(t)
	s, d, tok := newTestServer(t)
	ctx := context.Background()
	family(t, d, model.StatusIdle) // boss-1, kid-1, other-1
	dir := t.TempDir()
	for _, id := range []string{"porch-1", "porch-2"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: dir, Status: model.StatusIdle}); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.SetProject(ctx, "porch-1", "web")
	_ = d.SetProject(ctx, "porch-2", "docs")
	for id, r := range map[string]string{"boss-1": model.RoleOrchestrator, "porch-1": model.RoleProjectOrchestrator, "porch-2": model.RoleProjectOrchestrator} {
		if _, err := d.SetRole(ctx, id, r, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		to, from string
		want     int
	}{
		{"porch-1", "boss-1", 200},                   // global → project
		{"boss-1", "porch-1", 200},                   // project → global
		{"porch-2", "porch-1", http.StatusForbidden}, // project → project
		{"porch-1", "other-1", http.StatusForbidden}, // no role
		{"other-1", "boss-1", http.StatusForbidden},  // an orchestrator → a stranger
		{"porch-1", "kid-1", http.StatusForbidden},   // a worker → an orchestrator that is not its parent
	} {
		if code, out := tell(t, s, tok, c.to, c.from, "hello"); code != c.want {
			t.Errorf("%s → %s = %d %s, want %d", c.from, c.to, code, out.Note, c.want)
		}
	}

	// Across machines: the sending device says what its session is.
	s.senderOnPeer = func(_ context.Context, dev, id string) (string, string, error) {
		switch {
		case dev == "dev-A" && id == "far-orch":
			return "", model.RoleOrchestrator, nil
		case dev == "dev-A" && id == "far-porch":
			return "", model.RoleProjectOrchestrator, nil
		}
		return "", "", nil
	}
	devA := map[string]string{tunnel.HeaderPeerID: "dev-A", tunnel.HeaderPeerName: "laptop", tunnel.HeaderPeerLetter: "A"}
	devB := map[string]string{tunnel.HeaderPeerID: "dev-B", tunnel.HeaderPeerName: "other", tunnel.HeaderPeerLetter: "C"}
	for _, c := range []struct {
		to, from string
		hdr      map[string]string
		want     int
	}{
		{"porch-1", "far-orch", devA, 200},
		{"boss-1", "far-porch", devA, 200},
		{"porch-1", "far-orch", devB, http.StatusForbidden},  // dev-B has no such session
		{"porch-1", "far-porch", devA, http.StatusForbidden}, // project → project
		{"other-1", "far-orch", devA, http.StatusForbidden},  // the target is no orchestrator
		{"kid-1", "far-orch", devA, http.StatusForbidden},
	} {
		rec := throughHub(s, false, c.to, map[string]string{"text": "hello", "from_session": c.from, "from_ref": "7"}, c.hdr)
		if rec.Code != c.want {
			t.Errorf("%s (%s) → %s through the hub = %d %s, want %d", c.from, c.hdr[tunnel.HeaderPeerID], c.to, rec.Code, rec.Body, c.want)
		}
	}
}
