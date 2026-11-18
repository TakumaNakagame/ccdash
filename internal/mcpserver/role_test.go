package mcpserver

import (
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
	"github.com/TakumaNakagame/ccdash/internal/store"
)

// TestOrchestratorInstructionsFit: every orchestrator variant stays under
// InstructionsMax by itself, opens with the same two rules, and still fits
// Claude Code's cut with the longest operator text the setting takes and
// the longest project name the rules quote.
func TestOrchestratorInstructionsFit(t *testing.T) {
	longProject := strings.Repeat("プ", roleProjectMax+40)
	own := strings.Repeat("あ", settings.OrchestratorPromptMax)
	longest, longestOwn := 0, 0
	for _, role := range []string{model.RoleOrchestrator, model.RoleProjectOrchestrator} {
		for _, ro := range []bool{false, true} {
			for _, multi := range []bool{false, true} {
				for _, links := range []bool{false, true} {
					name := fmt.Sprintf("%s readOnly=%v multi=%v links=%v", role, ro, multi, links)
					text := orchestratorInstructions(role, longProject, ro, multi, links, "")
					n := chars(text)
					longest = max(longest, n)
					if n > InstructionsMax {
						t.Errorf("%s: %d characters, over InstructionsMax (%d)", name, n, InstructionsMax)
					}
					if !strings.HasPrefix(text, "Status rule") {
						t.Errorf("%s: the status rule does not open the instructions", name)
					}
					msg, end, rule := strings.Index(text, "Message rule"), strings.Index(text, "they answer there."), strings.Index(text, "Orchestrator rule")
					if msg < 0 || end < msg || rule < end {
						t.Errorf("%s: rules out of order (message %d, end %d, orchestrator %d)", name, msg, end, rule)
					}
					if got := chars(text[:end]); got > RulesWithin {
						t.Errorf("%s: the two rules end at character %d, past RulesWithin (%d)", name, got, RulesWithin)
					}
					full := orchestratorInstructions(role, longProject, ro, multi, links, own)
					m := chars(full)
					longestOwn = max(longestOwn, m)
					if m > InstructionsCut {
						t.Errorf("%s: %d characters with the longest orchestrator_prompt (%d), over Claude Code's cut (%d): lower settings.OrchestratorPromptMax or tighten the rules", name, m, settings.OrchestratorPromptMax, InstructionsCut)
					}
					if !strings.HasSuffix(full, OwnRulesHeading+own) {
						t.Errorf("%s: the operator's own rules do not come last", name)
					}
				}
			}
		}
	}
	t.Logf("longest orchestrator variant: %d characters; with a %d-character orchestrator_prompt: %d (cut %d)", longest, settings.OrchestratorPromptMax, longestOwn, InstructionsCut)

	// The rules keep their meaning.
	full := orchestratorInstructions(model.RoleOrchestrator, "", false, true, true, "")
	for _, want := range []string{
		"Never investigate, edit or run things yourself", "nor through\nsubagents", "start_session",
		"goal / background + links / what to do / done when", "don't retype them", "list_projects",
		"ref and url", "my_sessions (since_last)", "the way its reach says",
		"Never pass on an approval or a permission", "give its url",
		"A project with its own orchestrator", "unless the operator says\n  otherwise",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("the global orchestrator's rules lost %q", want)
		}
	}
	proj := orchestratorInstructions(model.RoleProjectOrchestrator, "web", false, true, true, "")
	for _, want := range []string{`project "web" only`, `tell_session "orchestrator" is the global`, "Never investigate, edit or run things yourself"} {
		if !strings.Contains(proj, want) {
			t.Errorf("the project orchestrator's rules lack %q", want)
		}
	}
	if strings.Contains(proj, "A project with its own orchestrator") {
		t.Error("the project orchestrator is told to route to project orchestrators")
	}
	if strings.Contains(orchestratorInstructions(model.RoleProjectOrchestrator, longProject, false, false, false, ""), longProject) {
		t.Error("a long project name is quoted whole")
	}
	// A read-only server can't start sessions and must not say it can.
	ro := orchestratorInstructions(model.RoleOrchestrator, "", true, false, false, "")
	if strings.Contains(ro, "start_session") || !strings.Contains(ro, "--read-only") {
		t.Errorf("the read-only orchestrator text: %q", ro[strings.Index(ro, "Orchestrator rule"):])
	}
	// Every tool an orchestrator's instructions name exists.
	for _, readOnly := range []bool{false, true} {
		s := New(store.NewRemote("http://127.0.0.1:1", "x"), "test", readOnly)
		have := map[string]bool{}
		for _, tl := range s.tools() {
			have[tl["name"].(string)] = true
		}
		text := orchestratorInstructions(model.RoleOrchestrator, "", readOnly, true, true, "")
		for name := range allToolNames() {
			if strings.Contains(text, name) && !have[name] && name != "tell_session" {
				t.Errorf("readOnly=%v: the orchestrator instructions name %s, which this server does not offer", readOnly, name)
			}
		}
	}
}

// roleCollector plays one collector with the three levels: the global
// orchestrator "orch" (#1), project web's orchestrator "porch" (#2,
// running, child of orch), a worker "worker" (#3, child of porch, waiting
// on the operator), the operator's own session in web "manual" (#4), a
// stopped orchestrator of project docs "dorch" (#5) and a stranger (#6).
type roleCollector struct {
	settings map[string]string
	starts   []map[string]any
	told     []map[string]any
	seen     int
}

func (c *roleCollector) sessions() []model.Session {
	return []model.Session{
		{SessionID: "orch", Num: 1, Cwd: "/w", Status: model.StatusActive, Role: model.RoleOrchestrator},
		{SessionID: "porch", Num: 2, Cwd: "/w/web", Status: model.StatusIdle, Role: model.RoleProjectOrchestrator, Project: "web", Parent: "orch"},
		{SessionID: "worker", Num: 3, Cwd: "/w/web", Status: model.StatusIdle, Project: "web", Parent: "porch", Attention: model.AttentionNeedsYou, AttentionReason: "回答待ち: どちら"},
		{SessionID: "manual", Num: 4, Cwd: "/w/web", Status: model.StatusActive, Project: "web"},
		{SessionID: "dorch", Num: 5, Cwd: "/w/docs", Status: model.StatusStopped, Role: model.RoleProjectOrchestrator, Project: "docs"},
		{SessionID: "else", Num: 6, Cwd: "/w/e", Status: model.StatusIdle},
	}
}

func (c *roleCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	j := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch p := r.URL.Path; {
	case p == "/api/settings":
		j(c.settings)
	case p == "/api/sessions":
		if r.URL.Query().Get("archived") == "1" {
			j([]model.Session{})
			return
		}
		j(c.sessions())
	case p == "/api/projects":
		j([]model.Project{{Name: "web", Dir: "/w/web", Orchestrator: "porch"}, {Name: "docs", Dir: "/w/docs", Orchestrator: "dorch"}, {Name: "misc", Dir: "/w/misc"}})
	case p == "/api/peers":
		j(map[string]string{"porch": "web-orch-a1", "orch": "orch-b2"})
	case p == "/api/start-info":
		j(store.StartInfo{Dir: "/w/" + r.URL.Query().Get("project")})
	case p == "/pty/":
		j([]store.PTY{})
	case p == "/pty/start":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		c.starts = append(c.starts, b)
		j(map[string]any{"ptyKey": "pid-42", "tag": 1000001, "orchestrated": true, "notes": []string{}, "cwd": "/w/x"})
	case strings.HasSuffix(p, "/children-seen"):
		c.seen++
		j(map[string]any{})
	case strings.HasPrefix(p, "/api/sessions/") && strings.HasSuffix(p, "/message") && r.Method == "POST":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		b["to"] = strings.Split(p, "/")[3]
		c.told = append(c.told, b)
		j(map[string]any{"id": 1, "state": "delivered"})
	default:
		http.NotFound(w, r)
	}
}

func roleServer(t *testing.T, self string) (*Server, *roleCollector) {
	t.Helper()
	c := &roleCollector{settings: map[string]string{"attach_enabled": "1"}}
	ts := httptest.NewServer(c)
	t.Cleanup(ts.Close)
	s := New(store.NewRemote(ts.URL, "tok"), "test", false).WithHubURL("https://hub.example")
	s.self = func() string { return self }
	s.letter = func() string { return "" }
	s.roleEnv = nil
	return s, c
}

// TestRoleInInstructions: the server reads its own session's role — from
// the environment the collector set, else from the row — and its
// instructions carry the role's rules and the operator's own text.
func TestRoleInInstructions(t *testing.T) {
	s, c := roleServer(t, "else")
	if got := s.instructions(); got != instructionsFor(false, false, true) {
		t.Error("a session without a role does not get the plain instructions")
	}
	c.settings["orchestrator_prompt"] = "新しい仕事は既存のプロジェクトに入れる。"
	if got := s.instructions(); strings.Contains(got, "Orchestrator rule") || strings.Contains(got, "新しい仕事") {
		t.Error("a session without a role got the orchestrator's rules")
	}

	// From the row: a claude ccdash didn't start, or one whose role was
	// given while it ran.
	s, c = roleServer(t, "orch")
	c.settings["orchestrator_prompt"] = "新しい仕事は既存のプロジェクトに入れる。"
	got := s.instructions()
	if want := orchestratorInstructions(model.RoleOrchestrator, "", false, false, true, "新しい仕事は既存のプロジェクトに入れる。"); got != want {
		t.Errorf("the global orchestrator's instructions:\n%s", got)
	}
	if !strings.HasSuffix(got, OwnRulesHeading+"新しい仕事は既存のプロジェクトに入れる。") || !strings.Contains(got, "A project with its own orchestrator") {
		t.Errorf("the global orchestrator's instructions end with:\n%s", got[len(got)-300:])
	}
	s, _ = roleServer(t, "porch")
	if got := s.instructions(); !strings.Contains(got, `project "web" only`) || strings.Contains(got, OwnRulesHeading) {
		t.Errorf("the project orchestrator's instructions:\n%s", got)
	}

	// From the environment: a brand-new session has no row yet.
	s, _ = roleServer(t, "not-registered-yet")
	s.roleEnv = func() (string, string) { return model.RoleProjectOrchestrator, "docs" }
	if got := s.instructions(); !strings.Contains(got, `project "docs" only`) {
		t.Errorf("role from the environment:\n%s", got)
	}
	// …and the real reader takes nothing but the two roles.
	for env, want := range map[string]string{"orchestrator": model.RoleOrchestrator, "project-orchestrator": model.RoleProjectOrchestrator, "admin": "", "": ""} {
		t.Setenv("CCDASH_ROLE", env)
		t.Setenv("CCDASH_ROLE_PROJECT", "web")
		role, project := envRole()
		if role != want || (project != "") != (want == model.RoleProjectOrchestrator) {
			t.Errorf("CCDASH_ROLE=%q → %q / %q", env, role, project)
		}
	}

	// What initialize sends is that text, whole.
	t.Setenv("CCDASH_ROLE", "")
	s, c = roleServer(t, "orch")
	c.settings["orchestrator_prompt"] = strings.Repeat("あ", settings.OrchestratorPromptMax)
	var out strings.Builder
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n"
	if err := s.Serve(t.Context(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Result struct{ Instructions string }
	}
	if err := json.Unmarshal([]byte(out.String()), &reply); err != nil {
		t.Fatal(err)
	}
	if n := chars(reply.Result.Instructions); n > InstructionsCut || !strings.HasSuffix(reply.Result.Instructions, "あ") {
		t.Errorf("initialize sent %d characters (cut %d)", n, InstructionsCut)
	}
}

// TestNoToolSetsRole: a session must not be able to make an orchestrator —
// no tool takes a role, and nothing in this package calls the role setters.
func TestNoToolSetsRole(t *testing.T) {
	for _, list := range [][]map[string]any{readTools, writeTools, taskReadTools, taskWriteTools, {statusTool}} {
		for _, tool := range list {
			name, _ := tool["name"].(string)
			if strings.Contains(name, "role") || strings.Contains(name, "orchestrator") {
				t.Errorf("tool %s looks like it sets a role", name)
			}
			props, _ := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
			for arg := range props {
				if arg == "role" || strings.Contains(arg, "orchestrator") {
					t.Errorf("tool %s takes %q", name, arg)
				}
			}
		}
	}
	files, _ := filepath.Glob("*.go")
	if len(files) == 0 {
		t.Fatal("no source files found")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range []string{".SetRole(", ".SetProjectOrchestrator(", "KeyOrchestratorPrompt"} {
			if strings.Contains(string(b), call) {
				t.Errorf("%s uses %s: ccdash mcp must not give a role or write the orchestrator's instructions", f, call)
			}
		}
	}
	// start_session never sends a role, whatever it is called with.
	s, c := roleServer(t, "else")
	fastSpawn(t)
	if _, err := s.call(t.Context(), "start_session", json.RawMessage(`{"cwd":"/w/x","prompt":"hi","role":"orchestrator"}`)); err != nil {
		t.Fatal(err)
	}
	if b := c.starts[0]; b["role"] != nil || b["orchestrated"] != true {
		t.Errorf("start_session sent %v", b)
	}
}

// TestListProjectsOrchestrator: a project's row names its orchestrator and
// how to reach it.
func TestListProjectsOrchestrator(t *testing.T) {
	s, _ := roleServer(t, "orch")
	var out struct {
		Projects []struct {
			Name         string
			Orchestrator *orchRef
		}
	}
	if err := json.Unmarshal([]byte(call(t, s, "list_projects", `{}`)), &out); err != nil {
		t.Fatal(err)
	}
	by := map[string]*orchRef{}
	for _, p := range out.Projects {
		by[p.Name] = p.Orchestrator
	}
	if o := by["web"]; o == nil || o.Ref != "#2" || o.Reach != "SendMessage:web-orch-a1" || o.URL != "https://hub.example/s/porch" || o.Status != "idle" {
		t.Errorf("web orchestrator = %+v", o)
	}
	if o := by["docs"]; o == nil || o.Ref != "#5" || o.Reach != "tell_session" || o.Peer != "" || o.Status != "stopped" {
		t.Errorf("docs orchestrator = %+v", o)
	}
	if by["misc"] != nil {
		t.Errorf("misc has an orchestrator: %+v", by["misc"])
	}
	// list_sessions rows carry the role too.
	if raw := call(t, s, "list_sessions", `{}`); !strings.Contains(raw, `"role": "project-orchestrator"`) || !strings.Contains(raw, `"role": "orchestrator"`) {
		t.Errorf("list_sessions lacks roles: %s", raw)
	}
}

// TestStartSessionRouted: the global orchestrator's start_session for a
// project that has a running orchestrator starts nothing and names that
// orchestrator; direct: true, a stopped orchestrator, another project, and
// any other caller start as usual.
func TestStartSessionRouted(t *testing.T) {
	fastSpawn(t)
	s, c := roleServer(t, "orch")
	raw := call(t, s, "start_session", `{"project":"web","prompt":"ログイン画面を直す"}`)
	var routed struct {
		Routed              bool
		ProjectOrchestrator orchRef `json:"project_orchestrator"`
		Note                string
	}
	if err := json.Unmarshal([]byte(raw), &routed); err != nil {
		t.Fatal(err)
	}
	if !routed.Routed || len(c.starts) != 0 {
		t.Fatalf("routed = %v, %d starts: %s", routed.Routed, len(c.starts), raw)
	}
	if o := routed.ProjectOrchestrator; o.Ref != "#2" || o.Reach != "SendMessage:web-orch-a1" || o.URL != "https://hub.example/s/porch" {
		t.Errorf("project_orchestrator = %+v", o)
	}
	if !strings.Contains(routed.Note, "direct: true") || !strings.Contains(routed.Note, "nothing was started") || !strings.Contains(routed.Note, "web-orch-a1") {
		t.Errorf("note = %q", routed.Note)
	}

	call(t, s, "start_session", `{"project":"web","prompt":"ログイン画面を直す","direct":true}`)
	if len(c.starts) != 1 || c.starts[0]["project"] != "web" || c.starts[0]["direct"] != nil {
		t.Fatalf("direct start: %v", c.starts)
	}
	// A stopped orchestrator: started, and mentioned.
	if raw := call(t, s, "start_session", `{"project":"docs","prompt":"x"}`); len(c.starts) != 2 || !strings.Contains(raw, `"project_orchestrator_note"`) || strings.Contains(raw, `"routed"`) {
		t.Errorf("stopped orchestrator: %d starts, %s", len(c.starts), raw)
	}
	// No orchestrator / no project.
	for _, args := range []string{`{"project":"misc","prompt":"x"}`, `{"cwd":"/w/e","prompt":"x"}`} {
		if raw := call(t, s, "start_session", args); strings.Contains(raw, "routed") || strings.Contains(raw, "project_orchestrator") {
			t.Errorf("%s: %s", args, raw)
		}
	}
	if len(c.starts) != 4 {
		t.Errorf("%d starts, want 4", len(c.starts))
	}

	// The project's own orchestrator, and an ordinary session, start in
	// the project themselves.
	for _, self := range []string{"porch", "else"} {
		s, c := roleServer(t, self)
		if raw := call(t, s, "start_session", `{"project":"web","prompt":"x"}`); len(c.starts) != 1 || strings.Contains(raw, "routed") {
			t.Errorf("%s: %d starts, %s", self, len(c.starts), raw)
		}
	}
	// The role in the environment is enough (a new orchestrator, no row).
	s, c = roleServer(t, "not-registered-yet")
	s.roleEnv = func() (string, string) { return model.RoleOrchestrator, "" }
	if raw := call(t, s, "start_session", `{"project":"web","prompt":"x"}`); len(c.starts) != 0 || !strings.Contains(raw, `"routed": true`) {
		t.Errorf("role from the environment: %d starts, %s", len(c.starts), raw)
	}
}

// TestTellBetweenOrchestrators: the global orchestrator reaches a project's
// orchestrator, that one reaches the global one as "orchestrator"; nobody
// else gains anything.
func TestTellBetweenOrchestrators(t *testing.T) {
	s, c := roleServer(t, "orch")
	call(t, s, "tell_session", `{"session":"#5","text":"docs の依頼です"}`) // not its child
	if len(c.told) != 1 || c.told[0]["to"] != "dorch" || c.told[0]["from_session"] != "orch" {
		t.Fatalf("global → project: %v", c.told)
	}
	if msg := callErr(t, s, "tell_session", `{"session":"#4","text":"x"}`); !strings.Contains(msg, "neither your child nor your parent") {
		t.Errorf("global → a worker that is not its child: %s", msg)
	}
	if msg := callErr(t, s, "tell_session", `{"session":"orchestrator","text":"x"}`); !strings.Contains(msg, "not a project's orchestrator") {
		t.Errorf(`"orchestrator" from the global one: %s`, msg)
	}

	s, c = roleServer(t, "dorch") // no parent: found by role
	raw := call(t, s, "tell_session", `{"session":"orchestrator","text":"終わりました"}`)
	if len(c.told) != 1 || c.told[0]["to"] != "orch" || !strings.Contains(raw, `"to": "#1"`) {
		t.Fatalf("project → global: %v %s", c.told, raw)
	}
	if msg := callErr(t, s, "tell_session", `{"session":"#2","text":"x"}`); !strings.Contains(msg, "neither your child nor your parent") {
		t.Errorf("project orchestrator → another project's: %s", msg)
	}

	s, c = roleServer(t, "else")
	for _, to := range []string{"#1", "#2", "orchestrator"} {
		callErr(t, s, "tell_session", `{"session":"`+to+`","text":"x"}`)
	}
	if len(c.told) != 0 {
		t.Errorf("an ordinary session reached an orchestrator: %v", c.told)
	}
}

// TestMySessionsForOrchestrators: a project's orchestrator also sees the
// project's sessions it did not start; the global one gets each project
// orchestrator's children in numbers, with the ones waiting on the operator.
func TestMySessionsForOrchestrators(t *testing.T) {
	s, _ := roleServer(t, "porch")
	now := time.Now().Add(time.Hour)
	s.now = func() time.Time { return now }
	var p struct {
		Children        int
		NeedsOperator   []childRow `json:"needs_operator"`
		ProjectSessions *struct {
			Project  string
			Sessions int
			Working  []childRow
			Counts   map[string]int
		} `json:"project_sessions"`
		Orchestrators []any
	}
	raw := call(t, s, "my_sessions", `{}`)
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if p.Children != 1 || refs(p.NeedsOperator) != "#3" {
		t.Errorf("children: %d, needs %s", p.Children, refs(p.NeedsOperator))
	}
	ps := p.ProjectSessions
	if ps == nil || ps.Project != "web" || ps.Sessions != 1 || refs(ps.Working) != "#4" || ps.Counts["working"] != 1 {
		t.Fatalf("project_sessions = %+v\n%s", ps, raw)
	}
	if p.Orchestrators != nil {
		t.Error("a project orchestrator got the global summary")
	}

	s, _ = roleServer(t, "orch")
	s.now = func() time.Time { return now }
	var g struct {
		Children      int
		Orchestrators []struct {
			Orchestrator  orchRef
			Project       string
			Children      map[string]int
			NeedsOperator []childRow `json:"needs_operator"`
		}
		ProjectSessions any `json:"project_sessions"`
	}
	raw = call(t, s, "my_sessions", `{}`)
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatal(err)
	}
	if g.Children != 1 || g.ProjectSessions != nil || len(g.Orchestrators) != 2 {
		t.Fatalf("global: %d children, %d orchestrators\n%s", g.Children, len(g.Orchestrators), raw)
	}
	docs, web := g.Orchestrators[0], g.Orchestrators[1]
	if docs.Project != "docs" || docs.Orchestrator.Ref != "#5" || docs.Children["needs_operator"] != 0 || len(docs.NeedsOperator) != 0 {
		t.Errorf("docs = %+v", docs)
	}
	if web.Project != "web" || web.Orchestrator.Ref != "#2" || web.Orchestrator.Reach != "SendMessage:web-orch-a1" || web.Children["needs_operator"] != 1 || refs(web.NeedsOperator) != "#3" || web.NeedsOperator[0].URL != "https://hub.example/s/worker" {
		t.Errorf("web = %+v", web)
	}

	// An ordinary parent gets neither.
	s, _ = roleServer(t, "else")
	if raw := call(t, s, "my_sessions", `{}`); strings.Contains(raw, "project_sessions") || strings.Contains(raw, "orchestrators") {
		t.Errorf("an ordinary session's my_sessions: %s", raw)
	}
}
