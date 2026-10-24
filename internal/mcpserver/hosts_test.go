package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/db"
	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/store"
	"github.com/TakumaNakagame/ccdash/internal/tasks"
)

// fakeHub plays the hub for this machine ("laptop", device A): it knows
// devices A, B ("server", online) and C ("old", offline), and serves B's
// collector routes itself from a tiny in-memory session list.
type fakeHub struct {
	mu      sync.Mutex
	peer    bool // peer access on
	seen    []string
	titles  map[string]string
	started []map[string]any
	attach  bool
	posts   map[string]string // B's task writes: path → body
}

// serverTasks is B's to-do list: #11 worked on by this machine's lap-1,
// and #3, which this machine has too.
func serverTasks() model.TaskList {
	return model.TaskList{Today: "2026-10-16", Buckets: []model.TaskBucket{{Key: "today", Heading: "今日"}}, Tasks: []model.Task{
		{ID: 11, Key: "INFRA-7", Title: "ノードを入れ替える", Priority: 2, Status: "doing", Bucket: "today", Sessions: []string{"lap-1"}},
		{ID: 3, Title: "B の 3", Priority: 3, Status: "todo", Bucket: "today"},
	}}
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer ccdh_tok" {
		http.Error(w, "unknown device token", 401)
		return
	}
	if !f.peer {
		http.Error(w, "peer access is off for this device (turn it on in the hub portal: 端末 → … → 他の端末へのアクセス)", 403)
		return
	}
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	j := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch p := r.URL.Path; {
	case p == "/api/peer/self":
		j(map[string]any{"id": "A", "name": "laptop", "peer_access": "control", "letter": "A"})
	case p == "/api/devices":
		j([]map[string]any{
			{"id": "A", "name": "laptop", "online": true, "letter": "A"},
			{"id": "B", "name": "server", "online": true, "letter": "B"},
			{"id": "C", "name": "old", "online": false, "letter": "C", "last_seen": time.Now().Add(-3 * time.Hour)},
		})
	case p == "/api/d/B/api/sessions":
		if r.URL.Query().Get("archived") == "1" {
			j([]model.Session{})
			return
		}
		j([]model.Session{
			{SessionID: "srv-1", Num: 1, HostLetter: "B", Cwd: "/srv/infra", Title: "k8s upgrade", CustomTitle: f.titles["srv-1"], Status: model.StatusIdle, LastSeen: time.Now()},
			{SessionID: "srv-2", Num: 2, HostLetter: "B", Cwd: "/srv/x", Title: "old", Status: model.StatusStopped, LastSeen: time.Now().Add(-time.Hour)},
		})
	case strings.HasPrefix(p, "/api/d/B/api/sessions/") && strings.HasSuffix(p, "/title"):
		var b struct{ Title string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.titles[strings.Split(p, "/")[6]] = b.Title
		j(map[string]any{})
	case p == "/api/d/B/hub/info":
		j(map[string]any{"attachEnabled": f.attach})
	case p == "/api/d/B/pty/start":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.started = append(f.started, b)
		j(map[string]any{"ptyKey": "pid-42"})
	case p == "/api/d/B/pty/":
		j([]any{})
	case p == "/api/d/B/api/projects", p == "/api/d/B/api/approvals":
		j([]any{})
	case p == "/api/d/B/api/tasks" && r.Method == "GET":
		j(serverTasks())
	case strings.HasPrefix(p, "/api/d/B/api/tasks/") && r.Method == "GET":
		for _, t := range serverTasks().Tasks {
			if p == "/api/d/B/api/tasks/"+strconv.FormatInt(t.ID, 10) {
				j(t)
				return
			}
		}
		http.Error(w, "no task", 404)
	case strings.HasPrefix(p, "/api/d/B/api/tasks") && r.Method == "POST":
		b, _ := io.ReadAll(r.Body)
		f.posts[strings.TrimPrefix(p, "/api/d/B")] = string(b)
		t := serverTasks().Tasks[0]
		if p == "/api/d/B/api/tasks" {
			t.ID = 12
		}
		j(t)
	case p == "/api/d/B/api/restart", p == "/api/d/B/api/usage":
		j(map[string]any{})
	default:
		_, _ = io.Copy(io.Discard, r.Body)
		http.NotFound(w, r)
	}
}

func multiHostServer(t *testing.T, peer bool) (*Server, *fakeHub, *db.DB) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // no real hub.json (hubcfg.Letter)
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for _, s := range []model.Session{
		{SessionID: "lap-1", Cwd: "/home/me/web", Title: "login page", Status: model.StatusIdle},
	} {
		if err := d.UpsertSession(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeHub{peer: peer, titles: map[string]string{}, attach: true, posts: map[string]string{}}
	hs := httptest.NewServer(f)
	t.Cleanup(hs.Close)
	s := New(store.NewLocal(d), "test", false).WithHubURL(hs.URL).WithFleet(NewFleet(hs.URL, "ccdh_tok"))
	s.self = func() string { return "" }
	s.letter = func() string { return "A" }
	return s, f, d
}

func call(t *testing.T, s *Server, name, args string) string {
	t.Helper()
	out, err := s.call(context.Background(), name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s %s: %v", name, args, err)
	}
	return out
}

func callErr(t *testing.T, s *Server, name, args string) string {
	t.Helper()
	_, err := s.call(context.Background(), name, json.RawMessage(args))
	if err == nil {
		t.Fatalf("%s %s: no error", name, args)
	}
	return err.Error()
}

func TestMultiHost(t *testing.T) {
	s, f, d := multiHostServer(t, true)
	ctx := context.Background()

	out := call(t, s, "list_hosts", `{}`)
	if !strings.Contains(out, `"server"`) || !strings.Contains(out, `"self": true`) {
		t.Errorf("list_hosts: %s", out)
	}
	// Every host by default; another host's rows carry host and <host>#N.
	out = call(t, s, "list_sessions", `{}`)
	for _, want := range []string{`"matched": 3`, `"ref": "A1"`, `"ref": "B1"`, `"host": "laptop"`, `"host": "server"`, `"offline_hosts"`, `"old"`} {
		if !strings.Contains(out, want) {
			t.Errorf("list_sessions lacks %s: %s", want, out)
		}
	}
	if out := call(t, s, "list_sessions", `{"host":"server"}`); !strings.Contains(out, `"matched": 2`) {
		t.Errorf("host=server: %s", out)
	}
	if out := call(t, s, "overview", `{}`); !strings.Contains(out, `"running": 2`) {
		t.Errorf("overview: %s", out)
	}

	// A bare #1 is this machine's; server#1 and host=server reach B.
	call(t, s, "set_title", `{"session":"#1","title":"here"}`)
	if x, _, _ := d.GetSession(ctx, "lap-1"); x.CustomTitle != "here" {
		t.Errorf("local title %q", x.CustomTitle)
	}
	call(t, s, "set_title", `{"session":"server#1","title":"there"}`)
	call(t, s, "set_title", `{"session":"2","host":"server","title":"two"}`)
	call(t, s, "set_title", `{"session":"srv-2","title":"by id"}`) // an id is searched everywhere
	if f.titles["srv-1"] != "there" || f.titles["srv-2"] != "by id" {
		t.Errorf("remote titles %v", f.titles)
	}
	if out := call(t, s, "get_session", `{"session":"server#1"}`); !strings.Contains(out, `"ref": "B1"`) {
		t.Errorf("get_session: %s", out)
	}
	// Letter refs: B2 is server's #2, a1 is this machine's #1.
	call(t, s, "set_title", `{"session":"B2","title":"by letter"}`)
	if f.titles["srv-2"] != "by letter" {
		t.Errorf("B2 title %v", f.titles)
	}
	call(t, s, "set_title", `{"session":"a1","title":"here again"}`)
	if x, _, _ := d.GetSession(ctx, "lap-1"); x.CustomTitle != "here again" {
		t.Errorf("a1 title %q", x.CustomTitle)
	}
	if e := callErr(t, s, "set_title", `{"session":"B1","host":"laptop","title":"x"}`); !strings.Contains(e, "host B") {
		t.Errorf("letter/host conflict: %s", e)
	}

	// Start on another host; the default stays this machine.
	spawnWait = 0
	out = call(t, s, "start_session", `{"host":"server","cwd":"/srv/infra","prompt":"hi"}`)
	if !strings.Contains(out, `"host": "server"`) || len(f.started) != 1 || f.started[0]["cwd"] != "/srv/infra" {
		t.Errorf("start on server: %s %v", out, f.started)
	}
	f.attach = false
	if e := callErr(t, s, "start_session", `{"host":"server","cwd":"/srv"}`); !strings.Contains(e, "OFF on host server") {
		t.Errorf("attach off: %s", e)
	}

	// Unknown / offline hosts say so.
	if e := callErr(t, s, "list_sessions", `{"host":"nope"}`); !strings.Contains(e, "hosts: laptop, old, server") {
		t.Errorf("unknown host: %s", e)
	}
	if e := callErr(t, s, "start_session", `{"host":"old","cwd":"/x"}`); !strings.Contains(e, "offline") {
		t.Errorf("offline host: %s", e)
	}
	if e := callErr(t, s, "set_title", `{"session":"server#9","title":"x"}`); !strings.Contains(e, "on host server") {
		t.Errorf("missing remote session: %s", e)
	}
	if e := callErr(t, s, "set_title", `{"session":"server#1","host":"laptop","title":"x"}`); !strings.Contains(e, "names host") {
		t.Errorf("conflicting host: %s", e)
	}
}

func TestMultiHostPeerOff(t *testing.T) {
	s, _, _ := multiHostServer(t, false)
	// Unchanged single-host behavior: no host fields, no extra hosts.
	out := call(t, s, "list_sessions", `{}`)
	if !strings.Contains(out, `"matched": 1`) || strings.Contains(out, `"host"`) {
		t.Errorf("peer off list: %s", out)
	}
	if e := callErr(t, s, "start_session", `{"host":"server","cwd":"/x"}`); !strings.Contains(e, "他の端末へのアクセス") {
		t.Errorf("peer off start: %s", e)
	}
	if out := call(t, s, "list_hosts", `{}`); !strings.Contains(out, "only this machine") {
		t.Errorf("peer off hosts: %s", out)
	}
	// "local" always names this machine.
	call(t, s, "set_title", `{"session":"#1","host":"local","title":"ok"}`)
}

func TestSplitRef(t *testing.T) {
	for in, want := range map[string][2]string{
		"#3": {"", "#3"}, "3": {"", "3"}, "server#3": {"server", "#3"}, "home lab#12": {"home lab", "#12"},
		"abc-def": {"", "abc-def"}, "x#y": {"", "x#y"},
	} {
		h, r := splitRef(in)
		if h != want[0] || r != want[1] {
			t.Errorf("splitRef(%q) = %q %q", in, h, r)
		}
	}
}

// TestMultiHostTasks: list_tasks merges the hosts' lists with "B#11" refs,
// refs and bare ids find the right host (this machine first), edits go to
// the task's own host by id, and session rows name tasks on other hosts.
func TestMultiHostTasks(t *testing.T) {
	s, f, d := multiHostServer(t, true)
	ctx := context.Background()
	for _, title := range []string{"ローカルの 1", "ローカルの 2", "ローカルの 3"} {
		if _, err := d.AddTask(ctx, tasks.Input{Title: title, Plan: "today"}); err != nil {
			t.Fatal(err)
		}
	}
	out := call(t, s, "list_tasks", `{}`)
	for _, want := range []string{`"ref": "A#1"`, `"ref": "B#11"`, `"host": "server"`, `"sessions": [
    "A1"`, `"matched": 5`} {
		if !strings.Contains(out, want) {
			t.Errorf("list_tasks lacks %s: %s", want, out)
		}
	}
	if out := call(t, s, "list_tasks", `{"host":"server"}`); strings.Contains(out, "A#1") || !strings.Contains(out, `"matched": 2`) {
		t.Errorf("list_tasks host=server: %s", out)
	}

	for args, want := range map[string]string{
		`{"task":"B#11"}`:         "ノードを入れ替える",
		`{"task":"#11"}`:          "ノードを入れ替える", // not here: the one host that has it
		`{"task":"INFRA-7"}`:      "ノードを入れ替える",
		`{"task":"#3"}`:           "ローカルの 3", // here first
		`{"task":"3","host":"B"}`: "B の 3",
		`{"task":"server#3"}`:     "B の 3",
		`{"task":"a#1"}`:          "ローカルの 1",
	} {
		if out := call(t, s, "get_task", args); !strings.Contains(out, want) {
			t.Errorf("get_task %s: want %q in %s", args, want, out)
		}
	}
	for args, want := range map[string]string{
		`{"task":"#99"}`:                 "not on the other hosts",
		`{"task":"C#1"}`:                 "offline",
		`{"task":"B#11","host":"local"}`: "is a task of host server",
	} {
		if _, err := s.call(ctx, "get_task", json.RawMessage(args)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("get_task %s: want %q, got %v", args, want, err)
		}
	}

	s.self = func() string { return "lap-1" }
	call(t, s, "add_task_note", `{"task":"B#11","text":"cp03 を入れ替えた"}`)
	call(t, s, "set_task_status", `{"task":"INFRA-7","status":"waiting","why":"CI 待ち"}`)
	call(t, s, "add_task", `{"title":"B に足す","host":"B"}`)
	if b := f.posts["/api/tasks/11/notes"]; !strings.Contains(b, "cp03") {
		t.Errorf("note went elsewhere: %v", f.posts)
	}
	if b := f.posts["/api/tasks/11"]; !strings.Contains(b, `"waiting"`) || !strings.Contains(b, "lap-1") {
		t.Errorf("status patch: %v", f.posts)
	}
	if b := f.posts["/api/tasks"]; !strings.Contains(b, "B に足す") || !strings.Contains(b, "lap-1") {
		t.Errorf("add_task on B: %v", f.posts)
	}
	if lt, _ := d.ListTasks(ctx, false); len(lt) != 3 {
		t.Errorf("a remote write touched this machine's list: %d tasks", len(lt))
	}
	if _, err := s.call(ctx, "order_tasks", json.RawMessage(`{"tasks":["A#1","B#11"]}`)); err == nil || !strings.Contains(err.Error(), "different hosts") {
		t.Errorf("order across hosts: %v", err)
	}

	if out := call(t, s, "list_sessions", `{"host":"local"}`); !strings.Contains(out, `"tasks": [
    "B#11"`) {
		t.Errorf("lap-1's row lacks B#11: %s", out)
	}
}
