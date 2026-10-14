package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/store"
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
		j(map[string]any{"id": "A", "name": "laptop", "peer_access": "control"})
	case p == "/api/devices":
		j([]map[string]any{
			{"id": "A", "name": "laptop", "online": true},
			{"id": "B", "name": "server", "online": true},
			{"id": "C", "name": "old", "online": false, "last_seen": time.Now().Add(-3 * time.Hour)},
		})
	case p == "/api/d/B/api/sessions":
		if r.URL.Query().Get("archived") == "1" {
			j([]model.Session{})
			return
		}
		j([]model.Session{
			{SessionID: "srv-1", Num: 1, Cwd: "/srv/infra", Title: "k8s upgrade", CustomTitle: f.titles["srv-1"], Status: model.StatusIdle, LastSeen: time.Now()},
			{SessionID: "srv-2", Num: 2, Cwd: "/srv/x", Title: "old", Status: model.StatusStopped, LastSeen: time.Now().Add(-time.Hour)},
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
	case p == "/api/d/B/api/restart", p == "/api/d/B/api/usage":
		j(map[string]any{})
	default:
		_, _ = io.Copy(io.Discard, r.Body)
		http.NotFound(w, r)
	}
}

func multiHostServer(t *testing.T, peer bool) (*Server, *fakeHub, *db.DB) {
	t.Helper()
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
	f := &fakeHub{peer: peer, titles: map[string]string{}, attach: true}
	hs := httptest.NewServer(f)
	t.Cleanup(hs.Close)
	s := New(store.NewLocal(d), "test", false).WithHubURL(hs.URL).WithFleet(NewFleet(hs.URL, "ccdh_tok"))
	s.self = func() string { return "" }
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
	for _, want := range []string{`"matched": 3`, `"ref": "#1"`, `"ref": "server#1"`, `"host": "laptop"`, `"host": "server"`, `"offline_hosts"`, `"old"`} {
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
	if out := call(t, s, "get_session", `{"session":"server#1"}`); !strings.Contains(out, `"ref": "server#1"`) {
		t.Errorf("get_session: %s", out)
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
