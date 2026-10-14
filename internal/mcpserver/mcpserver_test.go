package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/store"
)

func TestServe(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tp := filepath.Join(dir, "a.jsonl")
	_ = os.WriteFile(tp, []byte(
		`{"type":"user","timestamp":"2026-09-01T10:00:00Z","message":{"role":"user","content":"add a login page"}}`+"\n"+
			`{"type":"assistant","timestamp":"2026-09-01T10:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"Done: login.tsx"}]}}`+"\n"), 0o600)
	for _, s := range []model.Session{
		{SessionID: "a", Cwd: "/w/web", Title: "login page", TranscriptPath: tp, Status: model.StatusIdle},
		{SessionID: "b", Cwd: "/w/infra", Title: "k8s", Status: model.StatusStopped, LastSeen: time.Now().Add(-time.Hour)},
	} {
		if err := d.UpsertSession(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.SetProject(ctx, "a", "web")
	_ = d.SetAttention(ctx, "a", model.AttentionNeedsYou, "question")

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_sessions","arguments":{"project":"web"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_session","arguments":{"session":"#1"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"overview"}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":8,"method":"bogus"}`,
	}, "\n")
	var out bytes.Buffer
	if err := New(store.NewLocal(d), "test", false).Serve(ctx, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 8 { // the notification gets no reply
		t.Fatalf("got %d replies:\n%s", len(lines), out.String())
	}
	type reply struct {
		ID     int `json:"id"`
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Tools           []struct{ Name string }
			IsError         bool `json:"isError"`
			Content         []struct{ Text string }
		} `json:"result"`
		Error *struct{ Code int } `json:"error"`
	}
	r := make([]reply, len(lines))
	for i, l := range lines {
		if err := json.Unmarshal([]byte(l), &r[i]); err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
	}
	if r[0].Result.ProtocolVersion != "2025-03-26" {
		t.Errorf("initialize: %s", lines[0])
	}
	if len(r[1].Result.Tools) != 34 {
		t.Errorf("tools/list: %s", lines[1])
	}
	body := func(i int) string { return r[i].Result.Content[0].Text }
	if !strings.Contains(body(2), `"name": "web"`) || !strings.Contains(body(2), `"needs_you": 1`) {
		t.Errorf("list_projects: %s", body(2))
	}
	if !strings.Contains(body(3), `"matched": 1`) || !strings.Contains(body(3), "login page") {
		t.Errorf("list_sessions: %s", body(3))
	}
	if !strings.Contains(body(4), "add a login page") || !strings.Contains(body(4), "Done: login.tsx") {
		t.Errorf("get_session: %s", body(4))
	}
	if !strings.Contains(body(5), `"attention_reason": "question"`) {
		t.Errorf("overview: %s", body(5))
	}
	if !r[6].Result.IsError {
		t.Errorf("unknown tool: %s", lines[6])
	}
	if r[7].Error == nil || r[7].Error.Code != -32601 {
		t.Errorf("unknown method: %s", lines[7])
	}
}

func TestOrganize(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, id := range []string{"a", "b", "c"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: "/w", Status: model.StatusStopped}); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(store.NewLocal(d), "test", false)
	call := func(name, args string) (string, error) { return srv.call(ctx, name, json.RawMessage(args)) }
	if _, err := call("set_project", `{"sessions":["#1","2"],"project":"web"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("set_project", `{"sessions":["c"],"project":"api"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("set_project", `{"sessions":["#1","#99"],"project":"x"}`); err == nil {
		t.Fatal("unknown session accepted")
	}
	if out, err := call("rename_project", `{"from":"api","to":"web"}`); err != nil || !strings.Contains(out, "merged") {
		t.Fatalf("merge: %s %v", out, err)
	}
	if c, _, _ := d.GetSession(ctx, "c"); c.Project != "web" {
		t.Fatalf("c project %q", c.Project)
	}
	if _, err := call("set_title", `{"session":"#3","title":"API work"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("set_archived", `{"sessions":["#2"],"archived":true}`); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := d.GetSession(ctx, "b"); !b.Archived {
		t.Fatal("not archived")
	}
	if c, _, _ := d.GetSession(ctx, "c"); c.CustomTitle != "API work" {
		t.Fatalf("title %q", c.CustomTitle)
	}
	ro := New(store.NewLocal(d), "test", true)
	if _, err := ro.call(ctx, "set_project", json.RawMessage(`{"sessions":["#1"],"project":"z"}`)); err == nil {
		t.Fatal("read-only server accepted a write")
	}
	if len(ro.tools()) != 9 {
		t.Fatalf("read-only tools: %d", len(ro.tools()))
	}
}

// TestGridPinColorSeen: set_grid / set_pinned / set_color / mark_seen reach
// the DB, list_sessions grid_only follows tile order, and --read-only
// refuses them.
func TestGridPinColorSeen(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, id := range []string{"a", "b", "c"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: "/w", Status: model.StatusStopped}); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.SetAttention(ctx, "a", model.AttentionDone, "")
	srv := New(store.NewLocal(d), "test", false)
	call := func(name, args string) (string, error) { return srv.call(ctx, name, json.RawMessage(args)) }

	if _, err := call("set_grid", `{"sessions":["#3"],"grid":true}`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // grid_at is Unix ms: keep the tile order apart
	if _, err := call("set_grid", `{"sessions":["#1","#3"],"grid":true}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("set_grid", `{"sessions":["#1"]}`); err == nil {
		t.Fatal("set_grid without grid accepted")
	}
	out, err := call("list_sessions", `{"grid_only":true}`)
	if err != nil || !strings.Contains(out, `"matched": 2`) || !strings.Contains(out, `"on_grid": true`) ||
		strings.Index(out, `"ref": "#3"`) > strings.Index(out, `"ref": "#1"`) {
		t.Fatalf("grid_only = %s, %v", out, err) // #3 went on first, so it is the first tile
	}
	if _, err := call("set_grid", `{"sessions":["#3"],"grid":false}`); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := d.GetSession(ctx, "c"); c.Grid {
		t.Fatal("c still on the grid")
	}

	if _, err := call("set_pinned", `{"sessions":["#2"],"pinned":true}`); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := d.GetSession(ctx, "b"); !b.Favorite {
		t.Fatal("b not pinned")
	}

	if _, err := call("set_color", `{"sessions":["#2"],"color":"#123456"}`); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := d.GetSession(ctx, "b"); b.Color() != "#123456" {
		t.Fatalf("color %q", b.Color())
	}
	if _, err := call("set_color", `{"sessions":["#2"],"color":"red"}`); err == nil {
		t.Fatal("bad color accepted")
	}
	if _, err := call("set_color", `{"sessions":["#2"],"color":"random"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("set_color", `{"sessions":["#2"],"color":""}`); err != nil {
		t.Fatal(err)
	}

	if _, err := call("mark_seen", `{"sessions":["#1"]}`); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := d.GetSession(ctx, "a"); a.Attention == model.AttentionDone {
		t.Fatal("done mark not cleared")
	}

	ro := New(store.NewLocal(d), "test", true)
	for _, name := range []string{"set_grid", "set_pinned", "set_color", "mark_seen"} {
		if _, err := ro.call(ctx, name, json.RawMessage(`{"sessions":["#1"],"grid":true,"pinned":true,"color":"random"}`)); err == nil {
			t.Errorf("read-only server accepted %s", name)
		}
	}
}

func TestArchiveFinished(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	old := time.Now().Add(-48 * time.Hour)
	for _, s := range []model.Session{
		{SessionID: "run", Cwd: "/w", Status: model.StatusIdle},
		{SessionID: "old", Cwd: "/w", Status: model.StatusStopped, LastSeen: old},
		{SessionID: "fresh", Cwd: "/w", Status: model.StatusRecent, LastSeen: time.Now()},
		{SessionID: "pin", Cwd: "/w", Status: model.StatusStopped, LastSeen: old},
		{SessionID: "unread", Cwd: "/w", Status: model.StatusStopped, LastSeen: old},
	} {
		if err := d.UpsertSession(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.SetFavorite(ctx, "pin", true)
	_ = d.SetAttention(ctx, "unread", model.AttentionDone, "")
	srv := New(store.NewLocal(d), "test", false)
	call := func(args string) string {
		out, err := srv.call(ctx, "archive_finished", json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := call(`{"idle_hours":24,"dry_run":true}`)
	if !strings.Contains(out, `"archived": 1`) || !strings.Contains(out, `"session_id": "old"`) {
		t.Fatalf("dry run: %s", out)
	}
	if x, _, _ := d.GetSession(ctx, "old"); x.Archived {
		t.Fatal("dry run archived")
	}
	out = call(`{}`)
	if !strings.Contains(out, `"archived": 2`) || !strings.Contains(out, `"running"`) || !strings.Contains(out, `"pinned"`) {
		t.Fatalf("archive: %s", out)
	}
	for id, want := range map[string]bool{"run": false, "old": true, "fresh": true, "pin": false, "unread": false} {
		if x, _, _ := d.GetSession(ctx, id); x.Archived != want {
			t.Errorf("%s archived = %v", id, x.Archived)
		}
	}
	if _, err := New(store.NewLocal(d), "test", true).call(ctx, "archive_finished", nil); err == nil {
		t.Error("read-only: want error")
	}
}

func TestStartSession(t *testing.T) {
	attach := "1"
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/settings":
			_ = json.NewEncoder(w).Encode(map[string]string{"attach_enabled": attach})
		case "/pty/start":
			_ = json.NewDecoder(r.Body).Decode(&got)
			_, _ = w.Write([]byte(`{"ptyKey":"pid-42","tag":1000001}`))
		case "/pty/":
			_, _ = w.Write([]byte(`[{"key":"pid-42","alive":true,"pid":42,"cwd":"/w/new","prompt":"確認: Is this a project you trust?"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	s := New(store.NewRemote(ts.URL, "tok"), "test", false)
	ctx := context.Background()

	out, err := s.call(ctx, "start_session", json.RawMessage(`{"cwd":"/w/new/","project":"infra","prompt":"read task.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pid-42") || got["cwd"] != "/w/new" || got["project"] != "infra" || got["prompt"] != "read task.md" {
		t.Errorf("out=%s req=%v", out, got)
	}
	if !strings.Contains(out, `"waiting_on": "確認: Is this a project you trust?"`) {
		t.Errorf("trust dialog not reported: %s", out)
	}
	for _, bad := range []string{`{"cwd":"rel/dir"}`, `{"cwd":"/w","prompt":"-x"}`} {
		if _, err := s.call(ctx, "start_session", json.RawMessage(bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
	attach = "0"
	if _, err := s.call(ctx, "start_session", json.RawMessage(`{"cwd":"/w"}`)); err == nil || !strings.Contains(err.Error(), "OFF") {
		t.Errorf("attach off: %v", err)
	}
	if _, err := New(store.NewRemote(ts.URL, "tok"), "test", true).call(ctx, "start_session", json.RawMessage(`{"cwd":"/w"}`)); err == nil {
		t.Error("read-only: want error")
	}
}

// TestHubLinks: with a hub URL every session row links to its portal chat;
// without one there is no url field.
func TestHubLinks(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_ = d.UpsertSession(ctx, &model.Session{SessionID: "abc-1", Cwd: "/w", Status: model.StatusIdle})
	_ = d.SetAttention(ctx, "abc-1", model.AttentionNeedsYou, "question")
	out, err := New(store.NewLocal(d), "test", true).WithHubURL("https://hub.example/").call(ctx, "overview", nil)
	if err != nil || !strings.Contains(out, `"url": "https://hub.example/s/abc-1"`) {
		t.Fatalf("overview = %s, %v", out, err)
	}
	out, _ = New(store.NewLocal(d), "test", true).call(ctx, "get_session", json.RawMessage(`{"session":"#1"}`))
	if strings.Contains(out, `"url"`) {
		t.Fatalf("url without a hub: %s", out)
	}
}

func TestSetStatus(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := model.Session{SessionID: "me", Cwd: "/w", Status: model.StatusActive}
	if err := d.UpsertSession(ctx, &s); err != nil {
		t.Fatal(err)
	}
	srv := New(store.NewLocal(d), "test", true) // read-only keeps set_status
	srv.self = func() string { return "me" }
	if _, err := srv.call(ctx, "set_status", json.RawMessage(`{"status":"maybe","note":"x"}`)); err == nil {
		t.Fatal("accepted an unknown status")
	}
	if _, err := srv.call(ctx, "set_status", json.RawMessage(`{"status":"answer","note":"どちらの案で\n進めますか？"}`)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := d.GetSession(ctx, "me")
	if got.Wait != model.WaitAnswer || got.WaitNote != "どちらの案で 進めますか？" || got.WaitAt.IsZero() {
		t.Fatalf("wait %q %q %v", got.Wait, got.WaitNote, got.WaitAt)
	}
	srv.self = func() string { return "" }
	if _, err := srv.call(ctx, "set_status", json.RawMessage(`{"status":"report","note":"x"}`)); err == nil {
		t.Fatal("no session to set")
	}
	if _, err := srv.call(ctx, "set_status", json.RawMessage(`{"status":"report","note":"done","session":"me"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestParentOf(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	if got := parentOf(os.Getpid()); got != os.Getppid() {
		t.Fatalf("parentOf = %d, want %d", got, os.Getppid())
	}
}

// TestStopResumeSession: stop_session ends only a hosted, non-self, idle
// session (force for a working one); resume_session restarts only one that
// runs nowhere, by its id and cwd.
func TestStopResumeSession(t *testing.T) {
	attach := "1"
	var deleted []string
	var started map[string]any
	sessions := []model.Session{
		{SessionID: "host", Num: 1, Cwd: "/w/a", Status: model.StatusIdle},
		{SessionID: "busy", Num: 2, Cwd: "/w/b", Status: model.StatusActive},
		{SessionID: "term", Num: 3, Cwd: "/w/c", Status: model.StatusIdle},
		{SessionID: "done", Num: 4, Cwd: "/w/d", Status: model.StatusRecent},
		{SessionID: "me", Num: 5, Cwd: "/w/e", Status: model.StatusActive},
		{SessionID: "nocwd", Num: 6, Status: model.StatusStopped},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/settings":
			_ = json.NewEncoder(w).Encode(map[string]string{"attach_enabled": attach})
		case r.URL.Path == "/api/sessions":
			if r.URL.Query().Get("archived") == "1" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_ = json.NewEncoder(w).Encode(sessions)
		case r.URL.Path == "/pty/" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"key":"host","alive":true,"pid":10},{"key":"busy","alive":true,"pid":11},{"key":"me","alive":true,"pid":12},{"key":"pid-13","alive":true,"pid":13}]`))
		case r.Method == http.MethodDelete:
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/pty/"))
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/pty/start":
			_ = json.NewDecoder(r.Body).Decode(&started)
			_, _ = w.Write([]byte(`{"ptyKey":"done"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	s := New(store.NewRemote(ts.URL, "tok"), "test", false)
	s.self = func() string { return "me" }
	ctx := context.Background()
	call := func(name, args string) (string, error) { return s.call(ctx, name, json.RawMessage(args)) }

	if _, err := call("stop_session", `{"session":"#1"}`); err != nil || len(deleted) != 1 || deleted[0] != "host" {
		t.Fatalf("stop #1: err=%v deleted=%v", err, deleted)
	}
	for args, want := range map[string]string{
		`{"session":"#2"}`:  "mid-turn",
		`{"session":"#3"}`:  "outside ccdash",
		`{"session":"#4"}`:  "already stopped",
		`{"session":"#5"}`:  "your own session",
		`{"session":"#99"}`: "no session",
		`{}`:                "required",
	} {
		if _, err := call("stop_session", args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("stop %s: want %q, got %v", args, want, err)
		}
	}
	if _, err := call("stop_session", `{"session":"busy","force":true}`); err != nil || deleted[len(deleted)-1] != "busy" {
		t.Errorf("force stop: err=%v deleted=%v", err, deleted)
	}

	out, err := call("resume_session", `{"session":"#4","permission_mode":"plan","prompt":"go on"}`)
	if err != nil {
		t.Fatal(err)
	}
	if started["sessionId"] != "done" || started["resumeId"] != "done" || started["cwd"] != "/w/d" ||
		started["permissionMode"] != "plan" || started["prompt"] != "go on" || !strings.Contains(out, `"resumed": "#4"`) {
		t.Errorf("resume: out=%s req=%v", out, started)
	}
	for args, want := range map[string]string{
		`{"session":"#1"}`:                "already running under ccdash",
		`{"session":"#3"}`:                "still running outside ccdash",
		`{"session":"#6"}`:                "no known directory",
		`{"session":"#4","prompt":"-rf"}`: "must not start",
	} {
		if _, err := call("resume_session", args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("resume %s: want %q, got %v", args, want, err)
		}
	}

	attach = "0"
	for _, name := range []string{"stop_session", "resume_session"} {
		if _, err := call(name, `{"session":"#1"}`); err == nil || !strings.Contains(err.Error(), "OFF") {
			t.Errorf("%s with attach off: %v", name, err)
		}
		if _, err := New(store.NewRemote(ts.URL, "tok"), "test", true).call(ctx, name, json.RawMessage(`{"session":"#1"}`)); err == nil {
			t.Errorf("%s read-only: want error", name)
		}
	}
}

// TestScheduleRestart: schedule_restart posts {"when":"idle"} only to a
// collector that knows scheduled restarts (GET /api/restart answers);
// cancel deletes; overview shows a pending one.
func TestScheduleRestart(t *testing.T) {
	old := false
	var posted map[string]any
	pending := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/settings":
			_ = json.NewEncoder(w).Encode(map[string]string{"attach_enabled": "1"})
		case r.URL.Path == "/api/restart" && old:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		case r.URL.Path == "/api/restart" && r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&posted)
			pending = true
			fallthrough
		case r.URL.Path == "/api/restart":
			if r.Method == http.MethodDelete {
				pending = false
			}
			_ = json.NewEncoder(w).Encode(model.RestartState{Pending: pending, Hosted: 2, Waiting: []model.RestartBlocker{{SessionID: "x", Num: 7, Why: "作業中"}}})
		case r.URL.Path == "/api/sessions", r.URL.Path == "/api/approvals", r.URL.Path == "/api/projects", r.URL.Path == "/pty/":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	s := New(store.NewRemote(ts.URL, "tok"), "test", false)
	ctx := context.Background()
	out, err := s.call(ctx, "schedule_restart", json.RawMessage(`{"timeout_minutes":30}`))
	if err != nil || posted["when"] != "idle" || posted["by"] != "mcp" || posted["timeout_min"] != float64(30) || !strings.Contains(out, "#7 作業中") {
		t.Fatalf("schedule: err=%v posted=%v out=%s", err, posted, out)
	}
	if out, _ := s.call(ctx, "overview", nil); !strings.Contains(out, "restart_pending") {
		t.Errorf("overview without restart_pending: %s", out)
	}
	if out, err := s.call(ctx, "schedule_restart", json.RawMessage(`{"cancel":true}`)); err != nil || !strings.Contains(out, `"pending": false`) {
		t.Errorf("cancel: %v %s", err, out)
	}
	old, posted = true, nil
	if _, err := s.call(ctx, "schedule_restart", nil); err == nil || posted != nil {
		t.Errorf("old collector: err=%v posted=%v (must not POST: it would restart at once)", err, posted)
	}
}
