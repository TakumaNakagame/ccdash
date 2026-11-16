package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/db"
	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/store"
)

// fastSpawn shortens start_session's wait for the tests.
func fastSpawn(t *testing.T) {
	t.Helper()
	w, p := spawnWait, spawnPoll
	spawnWait, spawnPoll = 300*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { spawnWait, spawnPoll = w, p })
}

// TestStartSessionResult: start_session sends the caller as the parent and
// answers with the new session's ref, id, url and SendMessage name once the
// PTY has its session id and a row; until then it says pending.
func TestStartSessionResult(t *testing.T) {
	fastSpawn(t)
	var got map[string]any
	polls, upAfter := 0, 2
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/settings":
			_ = json.NewEncoder(w).Encode(map[string]string{"attach_enabled": "1"})
		case "/pty/start":
			_ = json.NewDecoder(r.Body).Decode(&got)
			_, _ = w.Write([]byte(`{"ptyKey":"pid-42","tag":1000001}`))
		case "/pty/":
			polls++
			rows := []store.PTY{{Key: "pid-42", Alive: true, PID: 42, Cwd: "/w/new", Parent: "boss"}}
			if upAfter >= 0 && polls > upAfter {
				rows = append(rows, store.PTY{Key: "kid-sid", Alive: true, PID: 42, Cwd: "/w/new", Parent: "boss"})
			}
			_ = json.NewEncoder(w).Encode(rows)
		case "/api/sessions":
			if r.URL.Query().Get("archived") == "1" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_ = json.NewEncoder(w).Encode([]model.Session{{SessionID: "kid-sid", Num: 7, Cwd: "/w/new", Status: model.StatusActive, Parent: "boss"}})
		case "/api/peers":
			// Only the name ever comes out of the collector.
			_ = json.NewEncoder(w).Encode(map[string]string{"kid-sid": "new-task-a1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	s := New(store.NewRemote(ts.URL, "tok"), "test", false).WithHubURL("https://hub.example")
	s.self = func() string { return "boss" }
	ctx := context.Background()

	out, err := s.call(ctx, "start_session", json.RawMessage(`{"cwd":"/w/new/","prompt":"read task.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got["parent"] != "boss" || got["parentHost"] != nil || got["cwd"] != "/w/new" {
		t.Errorf("request = %v", got)
	}
	for _, want := range []string{`"started": "pid-42"`, `"ref": "#7"`, `"session_id": "kid-sid"`, `"url": "https://hub.example/s/kid-sid"`, `"peer": "new-task-a1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %s: %s", want, out)
		}
	}
	if strings.Contains(out, "pending") {
		t.Errorf("a started session is not pending: %s", out)
	}

	// Never registers within the wait: pending, with the key to look for.
	polls, upAfter = 0, -1
	out, err = s.call(ctx, "start_session", json.RawMessage(`{"cwd":"/w/new"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"pending": true`) || !strings.Contains(out, `"started": "pid-42"`) || strings.Contains(out, `"ref"`) || !strings.Contains(out, "my_sessions") {
		t.Errorf("pending result: %s", out)
	}

	// No calling session: nothing claims to be the parent.
	s.self = func() string { return "" }
	polls, upAfter, got = 0, 0, nil
	if _, err := s.call(ctx, "start_session", json.RawMessage(`{"cwd":"/w/new"}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["parent"]; ok {
		t.Errorf("parent sent without a caller: %v", got)
	}
}

// orchestra is a local DB with a parent ("boss", #1), four children in the
// four my_sessions groups, a grandchild and an unrelated session.
func orchestra(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	tp := filepath.Join(dir, "done.jsonl")
	long := strings.Repeat("あ", 400)
	_ = os.WriteFile(tp, []byte(
		`{"type":"user","timestamp":"2026-09-01T10:00:00Z","message":{"role":"user","content":"build it"}}`+"\n"+
			`{"type":"assistant","timestamp":"2026-09-01T10:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"first reply"}]}}`+"\n"+
			`{"type":"assistant","timestamp":"2026-09-01T10:01:05Z","message":{"role":"assistant","content":[{"type":"text","text":"PR #12 is open. `+long+`"}]}}`+"\n"), 0o600)
	for _, x := range []model.Session{
		{SessionID: "boss", Cwd: "/w", Title: "orchestrator", Status: model.StatusActive},
		{SessionID: "ask", Cwd: "/w/a", Title: "asks", Status: model.StatusIdle},
		{SessionID: "busy", Cwd: "/w/b", Title: "works", Status: model.StatusActive},
		{SessionID: "done", Cwd: "/w/c", Title: "reports", Status: model.StatusStopped, TranscriptPath: tp},
		{SessionID: "old", Cwd: "/w/d", Title: "over", Status: model.StatusStopped},
		{SessionID: "grand", Cwd: "/w/e", Title: "grandchild", Status: model.StatusStopped},
		{SessionID: "else", Cwd: "/w/f", Title: "unrelated", Status: model.StatusStopped},
	} {
		if err := d.UpsertSession(ctx, &x); err != nil {
			t.Fatal(err)
		}
	}
	for kid, parent := range map[string]string{"ask": "boss", "busy": "boss", "done": "boss", "old": "boss", "grand": "busy"} {
		if err := d.SetParent(ctx, kid, parent, ""); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.SetWait(ctx, "ask", model.WaitAnswer, "どちらにしますか")
	_ = d.SetAttention(ctx, "ask", model.AttentionNeedsYou, "回答待ち: どちらにしますか")
	_ = d.SetAttention(ctx, "done", model.AttentionDone, "")
	_ = d.SetProject(ctx, "done", "web")
	for _, id := range []string{"old", "grand", "else"} {
		_ = d.MarkSeen(ctx, id)
	}
	s := New(store.NewLocal(d), "test", false).WithHubURL("https://hub.example")
	s.self = func() string { return "boss" }
	return s, d
}

type mySessionsOut struct {
	Children      int
	NeedsOperator []childRow `json:"needs_operator"`
	Working       []childRow
	Reported      []childRow
	Finished      []childRow
	Counts        map[string]int
	Unchanged     *int
	Since         string
	SinceNote     string `json:"since_note"`
	Parent        string
}

func mySessions(t *testing.T, s *Server, args string) mySessionsOut {
	t.Helper()
	var o mySessionsOut
	raw := call(t, s, "my_sessions", args)
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return o
}

func refs(rows []childRow) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Ref
	}
	return strings.Join(out, ",")
}

// TestMySessions: the caller's children come back in the four groups, with
// the start of the latest reply; since_last leaves out what hasn't changed.
func TestMySessions(t *testing.T) {
	s, d := orchestra(t)
	ctx := context.Background()
	base := time.Now().Add(time.Hour) // the calls happen "later" than the rows
	s.now = func() time.Time { return base }

	o := mySessions(t, s, `{}`)
	if o.Children != 4 || refs(o.NeedsOperator) != "#2" || refs(o.Working) != "#3" || refs(o.Reported) != "#4" || refs(o.Finished) != "#5" {
		t.Fatalf("groups: needs=%s working=%s reported=%s finished=%s (%d children)", refs(o.NeedsOperator), refs(o.Working), refs(o.Reported), refs(o.Finished), o.Children)
	}
	if o.Parent != "#1" || o.Counts["needs_operator"] != 1 || o.Counts["finished"] != 1 {
		t.Errorf("parent %q counts %v", o.Parent, o.Counts)
	}
	ask, done, busy := o.NeedsOperator[0], o.Reported[0], o.Working[0]
	if ask.Wait != model.WaitAnswer || ask.WaitNote != "どちらにしますか" || ask.URL != "https://hub.example/s/ask" || ask.Age == "" {
		t.Errorf("needs row: %+v", ask)
	}
	if !strings.HasPrefix(done.LastReply, "PR #12 is open.") || len([]rune(done.LastReply)) != lastReplyLen+1 || done.Project != "web" {
		t.Errorf("reported row: last_reply %d runes %q project %q", len([]rune(done.LastReply)), done.LastReply, done.Project)
	}
	if busy.Children != 1 { // "grand" is busy's child, counted, not listed
		t.Errorf("working row children = %d", busy.Children)
	}

	// Asked again with since_last: nothing changed since the first call.
	base = base.Add(time.Minute)
	o = mySessions(t, s, `{"since_last":true}`)
	if o.Unchanged == nil || *o.Unchanged != 4 || len(o.NeedsOperator)+len(o.Working)+len(o.Reported)+len(o.Finished) != 0 || o.Since == "" {
		t.Fatalf("since_last, nothing new: %+v", o)
	}
	// One child moves on; only it comes back.
	if err := d.UpsertSession(ctx, &model.Session{SessionID: "old", Cwd: "/w/d", Status: model.StatusActive, LastSeen: base.Add(time.Minute), LastActivity: base.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	base = base.Add(2 * time.Minute)
	o = mySessions(t, s, `{"since_last":true}`)
	if refs(o.Working) != "#5" || *o.Unchanged != 3 || len(o.NeedsOperator)+len(o.Reported)+len(o.Finished) != 0 {
		t.Fatalf("since_last, one changed: working=%s unchanged=%v %+v", refs(o.Working), *o.Unchanged, o)
	}

	// --read-only still offers it, since_last included: the bookmark is the
	// caller's own, like set_status.
	ro := New(store.NewLocal(d), "test", true)
	ro.self, ro.now = s.self, func() time.Time { return base.Add(time.Minute) }
	if o := mySessions(t, ro, `{"since_last":true}`); o.Unchanged == nil || *o.Unchanged != 4 {
		t.Errorf("read-only since_last: %+v", o)
	}

	// Always the caller: no session argument, and a clear error without one.
	if e := callErr(t, s, "my_sessions", `{"session":"#3"}`); !strings.Contains(e, "takes no session") {
		t.Errorf("session argument: %s", e)
	}
	s.self = func() string { return "" }
	if e := callErr(t, s, "my_sessions", `{}`); !strings.Contains(e, "can't tell which session is calling") {
		t.Errorf("no caller: %s", e)
	}
	// A session with no children and no earlier call.
	s.self = func() string { return "else" }
	if o := mySessions(t, s, `{"since_last":true}`); o.Children != 0 || o.SinceNote == "" {
		t.Errorf("childless: %+v", o)
	}
}

// TestAdoptRelease: adopt_session overwrites a parent (one session, or all
// of another session's children), release_session clears the caller's own,
// and archive_finished with children_only leaves everyone else's alone.
func TestAdoptRelease(t *testing.T) {
	s, d := orchestra(t)
	ctx := context.Background()
	parent := func(id string) string {
		x, _, _ := d.GetSession(ctx, id)
		return x.Parent
	}

	// A new orchestrator ("else", #7) takes over one session, then the rest.
	s.self = func() string { return "else" }
	call(t, s, "adopt_session", `{"session":"#6"}`) // grand, busy's child until now
	if parent("grand") != "else" {
		t.Fatalf("adopt one: parent = %q", parent("grand"))
	}
	out := call(t, s, "adopt_session", `{"from":"#1"}`)
	for _, id := range []string{"ask", "busy", "done", "old"} {
		if parent(id) != "else" {
			t.Errorf("adopt from: %s parent = %q (%s)", id, parent(id), out)
		}
	}
	if o := mySessions(t, s, `{}`); o.Children != 5 {
		t.Errorf("after adopting: %d children", o.Children)
	}
	for args, want := range map[string]string{
		`{}`:                             "either session",
		`{"session":"#2","from":"#1"}`:   "either session",
		`{"session":"#7"}`:               "own session",
		`{"from":"#1"}`:                  "no children",
		`{"from":"#7"}`:                  "own session",
		`{"session":"nope-nope-nope-1"}`: "no session",
	} {
		if e := callErr(t, s, "adopt_session", args); !strings.Contains(e, want) {
			t.Errorf("adopt %s: %s (want %q)", args, e, want)
		}
	}
	// A child can't adopt its parent.
	s.self = func() string { return "ask" }
	if e := callErr(t, s, "adopt_session", `{"session":"#7"}`); !strings.Contains(e, "loop") {
		t.Errorf("loop: %s", e)
	}
	// Only the parent releases.
	if e := callErr(t, s, "release_session", `{"session":"#3"}`); !strings.Contains(e, "not your child") {
		t.Errorf("release someone else's: %s", e)
	}
	s.self = func() string { return "else" }
	call(t, s, "release_session", `{"session":"#3"}`)
	if parent("busy") != "" {
		t.Errorf("release: parent = %q", parent("busy"))
	}
	if e := callErr(t, s, "release_session", `{"session":"#3"}`); !strings.Contains(e, "no parent") {
		t.Errorf("release twice: %s", e)
	}

	// children_only: "old" (finished, seen, else's child) goes; "boss"
	// (finished too, but nobody's child) stays.
	_ = d.TouchSession(ctx, "boss", model.StatusStopped)
	_ = d.MarkSeen(ctx, "boss")
	out = call(t, s, "archive_finished", `{"children_only":true}`)
	if x, _, _ := d.GetSession(ctx, "old"); !x.Archived {
		t.Errorf("child not archived: %s", out)
	}
	if x, _, _ := d.GetSession(ctx, "boss"); x.Archived {
		t.Errorf("children_only archived a session that isn't a child: %s", out)
	}
	s.self = func() string { return "" }
	if e := callErr(t, s, "archive_finished", `{"children_only":true}`); !strings.Contains(e, "can't tell which session is calling") {
		t.Errorf("children_only without a caller: %s", e)
	}
	// The organizing tools are off with --read-only.
	ro := New(store.NewLocal(d), "test", true)
	ro.self = func() string { return "else" }
	for _, name := range []string{"adopt_session", "release_session"} {
		if e := callErr(t, ro, name, `{"session":"#2"}`); !strings.Contains(e, "read-only") {
			t.Errorf("%s read-only: %s", name, e)
		}
	}
}

// TestBriefAndPeerRows: get_session quotes the kept brief (clipped) and
// carries the parent.
func TestBriefAndPeerRows(t *testing.T) {
	s, d := orchestra(t)
	ctx := context.Background()
	id, err := d.InsertBrief(ctx, model.Brief{PtyKey: "pid-1", Parent: "boss", Text: "目的: ログイン画面\n" + strings.Repeat("x", 2500)})
	if err != nil {
		t.Fatal(err)
	}
	_ = d.LinkBrief(ctx, id, "busy")
	var o struct {
		Session        sessionRow
		Brief          string
		BriefTruncated bool `json:"brief_truncated"`
	}
	if err := json.Unmarshal([]byte(call(t, s, "get_session", `{"session":"#3"}`)), &o); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(o.Brief, "目的: ログイン画面") || len([]rune(o.Brief)) != 2000 || !o.BriefTruncated || o.Session.Parent != "boss" {
		t.Errorf("brief %d runes truncated=%v parent=%q", len([]rune(o.Brief)), o.BriefTruncated, o.Session.Parent)
	}
	if out := call(t, s, "get_session", `{"session":"#2"}`); strings.Contains(out, `"brief`) {
		t.Errorf("a session with no brief shows one: %s", out)
	}
}

// TestOrchestrateAcrossHosts: a session started on another host records
// where its parent lives, my_sessions gathers children from every host
// (spawns that have no session yet included), and adopt reaches them.
func TestOrchestrateAcrossHosts(t *testing.T) {
	fastSpawn(t)
	s, f, d := multiHostServer(t, true)
	ctx := context.Background()
	s.self = func() string { return "lap-1" }

	f.ptys = []store.PTY{{Key: "pid-42", Alive: true, PID: 42, Cwd: "/srv/infra", Parent: "lap-1"}}
	out := call(t, s, "start_session", `{"host":"server","cwd":"/srv/infra","prompt":"hi"}`)
	if len(f.started) != 1 || f.started[0]["parent"] != "lap-1" || f.started[0]["parentHost"] != "A" {
		t.Fatalf("start on server: %v", f.started)
	}
	if !strings.Contains(out, `"pending": true`) {
		t.Errorf("start on server: %s", out)
	}
	// Still starting there: listed as working, by its key.
	o := mySessions(t, s, `{}`)
	if o.Children != 1 || len(o.Working) != 1 || o.Working[0].Key != "pid-42" || o.Working[0].Status != "starting" || o.Working[0].Host != "server" {
		t.Fatalf("starting spawn: %+v", o)
	}
	// Stuck on a dialog: the operator has to answer it.
	f.ptys[0].Prompt = "確認: Is this a project you trust?"
	if o := mySessions(t, s, `{}`); len(o.NeedsOperator) != 1 || o.NeedsOperator[0].WaitingOn == "" {
		t.Fatalf("stuck spawn: %+v", o)
	}
	f.ptys = nil

	// Adopt a session on the other host: its row says the parent is on A.
	call(t, s, "adopt_session", `{"session":"B1"}`)
	if f.parents["srv-1"] != "lap-1" || f.parentHosts["srv-1"] != "A" {
		t.Fatalf("adopt B1: %v %v", f.parents, f.parentHosts)
	}
	// And one here: no host.
	if err := d.UpsertSession(ctx, &model.Session{SessionID: "lap-2", Cwd: "/home/me/x", Status: model.StatusStopped}); err != nil {
		t.Fatal(err)
	}
	call(t, s, "adopt_session", `{"session":"lap-2"}`)
	if x, _, _ := d.GetSession(ctx, "lap-2"); x.Parent != "lap-1" || x.ParentHost != "" {
		t.Fatalf("adopt local: %q %q", x.Parent, x.ParentHost)
	}
	o = mySessions(t, s, `{}`)
	all := refs(o.NeedsOperator) + "|" + refs(o.Working) + "|" + refs(o.Reported) + "|" + refs(o.Finished)
	if o.Children != 2 || !strings.Contains(all, "B1") || !strings.Contains(all, "A2") {
		t.Fatalf("children across hosts: %s (%d)", all, o.Children)
	}
	call(t, s, "release_session", `{"session":"B1"}`)
	if f.parents["srv-1"] != "" {
		t.Errorf("release B1: %v", f.parents)
	}
}
