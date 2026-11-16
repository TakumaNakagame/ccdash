package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/db"
	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/plaintext"
	"github.com/TakumaNakagame/ccdash/internal/store"
)

// TestInstructionsMessageRule: Claude Code cuts a server's instructions
// after a few thousand characters, so the rule every session must read —
// what a "[ccdash 伝言]" message is and is not — has to open them.
func TestInstructionsMessageRule(t *testing.T) {
	head := []rune(instructions)
	if len(head) > 900 {
		head = head[:900]
	}
	for _, want := range []string{model.MessageMark, "not from the operator", "approval", "set_status action / answer", "tell_session", "SendMessage", "url"} {
		if !strings.Contains(string(head), want) {
			t.Errorf("the first 900 characters of the instructions lack %q", want)
		}
	}
	if !strings.HasPrefix(model.MessageHeader("laptop", "A12"), model.MessageMark+" ") {
		t.Errorf("the header %q does not start with the mark the rule names", model.MessageHeader("laptop", "A12"))
	}
}

// TestChildGroup pins the grouping, the caller's own "not shown yet"
// included.
func TestChildGroup(t *testing.T) {
	shown := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	before, after := shown.Add(-time.Minute), shown.Add(time.Minute)
	for _, c := range []struct {
		name string
		x    model.Session
		want string
	}{
		{"approval", model.Session{Status: model.StatusActive, PendingCount: 1}, groupNeeds},
		{"declared answer", model.Session{Status: model.StatusIdle, Wait: model.WaitAnswer, LastActivity: after}, groupNeeds},
		{"mid-turn", model.Session{Status: model.StatusActive, LastActivity: after}, groupWorking},
		{"background wait", model.Session{Status: model.StatusIdle, Wait: model.WaitBackground, LastActivity: after}, groupWorking},
		{"unread by the operator", model.Session{Status: model.StatusIdle, Attention: model.AttentionDone, LastActivity: before}, groupReported},
		{"NEW", model.Session{Status: model.StatusStopped, Unseen: true, LastActivity: before}, groupReported},
		// The observed case: finished, declared report, and a screen
		// already marked it seen — but its parent has not been shown it.
		{"seen by a screen, new to the caller", model.Session{Status: model.StatusIdle, Wait: model.WaitReport, WaitAt: after, AttentionAt: after, LastActivity: after}, groupReported},
		{"ended in the second of the last call", model.Session{Status: model.StatusIdle, LastActivity: shown}, groupReported},
		{"stopped since, seen", model.Session{Status: model.StatusRecent, LastActivity: after}, groupReported},
		{"seen and already shown", model.Session{Status: model.StatusIdle, Wait: model.WaitReport, WaitAt: before, AttentionAt: before, LastActivity: before}, groupFinished},
		{"nothing known", model.Session{Status: model.StatusStopped}, groupFinished},
	} {
		if got := childGroup(c.x, shown); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// TestMySessionsReportedOnceSeenByScreen reproduces what was seen on the
// live system: a child finished its turn (declared report), a ccdash screen
// that happened to have the row under it marked it seen a few seconds
// later, and its parent's my_sessions then filed it under finished, where a
// parent looking for reports never noticed it. It must be reported until
// the parent has been shown it once.
func TestMySessionsReportedOnceSeenByScreen(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for _, x := range []model.Session{
		{SessionID: "boss", Cwd: "/w", Status: model.StatusActive},
		{SessionID: "quick", Cwd: "/w/q", Title: "quick job", Status: model.StatusActive},
	} {
		if err := d.UpsertSession(ctx, &x); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SetParent(ctx, "quick", "boss", ""); err != nil {
		t.Fatal(err)
	}
	// finish is what the Stop hook of a turn that declared "report" leaves,
	// followed by a screen's /seen (MarkSeen).
	finish := func(at time.Time) {
		t.Helper()
		_ = d.SetWait(ctx, "quick", model.WaitReport, "終わりました")
		if err := d.UpsertSession(ctx, &model.Session{SessionID: "quick", Status: model.StatusIdle, LastActivity: at}); err != nil {
			t.Fatal(err)
		}
		_ = d.SetAttention(ctx, "quick", model.AttentionDone, "")
		if err := d.MarkSeen(ctx, "quick"); err != nil {
			t.Fatal(err)
		}
		x, _, _ := d.GetSession(ctx, "quick")
		if x.Attention != "" || x.Unseen || x.Status != model.StatusIdle || x.Wait != model.WaitReport {
			t.Fatalf("fixture: want an idle, seen row that declared report, got %+v", x)
		}
	}
	s := New(store.NewLocal(d), "test", false)
	s.self = func() string { return "boss" }
	// The calls happen a little after the rows' own (real-time) stamps.
	now := time.Now().Add(5 * time.Second)
	s.now = func() time.Time { return now }

	// The parent has never asked before.
	finish(now.Add(-10 * time.Second))
	o := mySessions(t, s, `{}`)
	if refs(o.Reported) != "#2" || len(o.Finished) != 0 {
		t.Fatalf("first call: reported=%s finished=%s, want the child under reported", refs(o.Reported), refs(o.Finished))
	}
	if o.Reported[0].Wait != model.WaitReport || o.Reported[0].Reach != "tell_session" {
		t.Errorf("row = %+v", o.Reported[0])
	}
	// Shown once: now it is done with.
	now = now.Add(time.Minute)
	if o := mySessions(t, s, `{}`); refs(o.Finished) != "#2" || len(o.Reported) != 0 {
		t.Fatalf("second call: reported=%s finished=%s, want finished", refs(o.Reported), refs(o.Finished))
	}
	// It works again and finishes again (seen by the screen again): news
	// once more — with since_last too.
	finish(now.Add(30 * time.Second))
	now = now.Add(time.Minute)
	if o := mySessions(t, s, `{"since_last":true}`); refs(o.Reported) != "#2" {
		t.Fatalf("after another turn: reported=%s finished=%s", refs(o.Reported), refs(o.Finished))
	}
	now = now.Add(time.Minute)
	if o := mySessions(t, s, `{}`); refs(o.Finished) != "#2" {
		t.Errorf("and done with again: reported=%s finished=%s", refs(o.Reported), refs(o.Finished))
	}
	// A turn that ended long before a first call is not dug up.
	s.self = func() string { return "quick" }
	_ = d.UpsertSession(ctx, &model.Session{SessionID: "old", Cwd: "/w/o", Status: model.StatusStopped, LastActivity: now.Add(-2 * time.Hour)})
	_ = d.SetParent(ctx, "old", "quick", "")
	_ = d.MarkSeen(ctx, "old")
	if o := mySessions(t, s, `{}`); refs(o.Finished) != "#3" || len(o.Reported) != 0 {
		t.Errorf("an old finished child on a first call: reported=%s finished=%s", refs(o.Reported), refs(o.Finished))
	}
}

// tellCollector plays one collector for the tell_session tests: a parent
// ("boss", #1), its children "kid" (#2, running, with a SendMessage name)
// and "nap" (#3, stopped), and an unrelated "else" (#4).
type tellCollector struct {
	mu     sync.Mutex
	attach string
	answer map[string]any
	told   []map[string]any
}

func (c *tellCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	j := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch p := r.URL.Path; {
	case p == "/api/settings":
		j(map[string]string{"attach_enabled": c.attach})
	case p == "/api/sessions":
		if r.URL.Query().Get("archived") == "1" {
			j([]model.Session{})
			return
		}
		j([]model.Session{
			{SessionID: "boss", Num: 1, Cwd: "/w", Status: model.StatusActive},
			{SessionID: "kid", Num: 2, Cwd: "/w/k", Status: model.StatusIdle, Parent: "boss"},
			{SessionID: "nap", Num: 3, Cwd: "/w/n", Status: model.StatusStopped, Parent: "boss"},
			{SessionID: "else", Num: 4, Cwd: "/w/e", Status: model.StatusIdle},
		})
	case p == "/api/peers":
		j(map[string]string{"kid": "kid-task-a1", "else": "else-b2"})
	case p == "/pty/":
		j([]store.PTY{})
	case strings.HasSuffix(p, "/children-seen"):
		j(map[string]any{})
	case strings.HasPrefix(p, "/api/sessions/") && strings.HasSuffix(p, "/message") && r.Method == "POST":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		b["to"] = strings.Split(p, "/")[3]
		c.told = append(c.told, b)
		j(c.answer)
	default:
		http.NotFound(w, r)
	}
}

func tellServer(t *testing.T) (*Server, *tellCollector) {
	t.Helper()
	c := &tellCollector{attach: "1", answer: map[string]any{"id": 1, "state": "delivered"}}
	ts := httptest.NewServer(c)
	t.Cleanup(ts.Close)
	s := New(store.NewRemote(ts.URL, "tok"), "test", false).WithHubURL("https://hub.example")
	s.self = func() string { return "boss" }
	s.letter = func() string { return "" }
	return s, c
}

// TestTellSession: only to a child or to the parent; the text rules; what
// the collector is handed; what comes back for each outcome.
func TestTellSession(t *testing.T) {
	s, c := tellServer(t)

	// A running child of this machine: delivered, with the hint that
	// SendMessage is the better channel there.
	out := call(t, s, "tell_session", `{"session":"#2","text":"  進み具合を教えてください  "}`)
	if len(c.told) != 1 {
		t.Fatalf("posted %d messages", len(c.told))
	}
	got := c.told[0]
	if got["to"] != "kid" || got["text"] != "進み具合を教えてください" || got["from_session"] != "boss" || got["from_ref"] != "#1" {
		t.Errorf("handed to the collector: %v", got)
	}
	if _, ok := got["from_host"]; ok {
		t.Errorf("the sender names its own machine (%v): that is the receiving side's to say", got["from_host"])
	}
	for _, want := range []string{`"result": "delivered"`, `"to": "#2"`, `"peer": "kid-task-a1"`, "SendMessage", `"url": "https://hub.example/s/kid"`, "not the operator's input"} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %s: %s", want, out)
		}
	}

	// A stopped child: resumed; no SendMessage name, so no hint.
	c.answer = map[string]any{"id": 2, "state": "resumed"}
	out = call(t, s, "tell_session", `{"session":"nap","text":"続きをお願いします"}`)
	if !strings.Contains(out, `"result": "resumed"`) || strings.Contains(out, "peer") || c.told[1]["to"] != "nap" {
		t.Errorf("stopped child: %s (%v)", out, c.told[1])
	}
	// Queued: the collector's reason is passed on.
	c.answer = map[string]any{"id": 3, "state": "queued", "note": "the session shows a dialog for the operator (確認: trust?)"}
	out = call(t, s, "tell_session", `{"session":"#2","text":"あとで"}`)
	if !strings.Contains(out, `"result": "queued"`) || !strings.Contains(out, "shows a dialog") || !strings.Contains(out, "don't send it again") {
		t.Errorf("queued: %s", out)
	}

	// The child reports to its parent — by "parent" or by ref.
	s.self = func() string { return "kid" }
	c.answer = map[string]any{"id": 4, "state": "delivered"}
	for _, arg := range []string{"parent", "#1"} {
		n := len(c.told)
		out = call(t, s, "tell_session", `{"session":"`+arg+`","text":"終わりました"}`)
		if len(c.told) != n+1 || c.told[n]["to"] != "boss" || c.told[n]["from_session"] != "kid" || c.told[n]["from_ref"] != "#2" || !strings.Contains(out, `"to": "#1"`) {
			t.Errorf("child → parent (%s): %s (%v)", arg, out, c.told)
		}
	}

	// Everyone else is refused here, before anything is sent.
	n := len(c.told)
	for args, want := range map[string]string{
		`{"session":"#4","text":"hi"}`:                                           "neither your child nor your parent", // unrelated
		`{"session":"#3","text":"hi"}`:                                           "neither your child nor your parent", // a sibling
		`{"session":"#2","text":"hi"}`:                                           "your own session",
		`{"session":"#1","text":"/clear"}`:                                       "must not start with",
		`{"session":"#1","text":"!ls"}`:                                          "must not start with",
		`{"session":"#1","text":"-p x"}`:                                         "must not start with",
		`{"session":"#1","text":"  "}`:                                           "empty",
		`{"session":"#1","text":"a\u001bb"}`:                                     "control characters",
		`{"session":"","text":"hi"}`:                                             "session is required",
		`{"session":"#99","text":"hi"}`:                                          "no session",
		`{"session":"#1","text":"` + strings.Repeat("a", plaintext.Max+1) + `"}`: "too long",
	} {
		if e := callErr(t, s, "tell_session", args); !strings.Contains(e, want) {
			t.Errorf("%.60s: %q, want %q", args, e, want)
		}
	}
	s.self = func() string { return "else" }
	if e := callErr(t, s, "tell_session", `{"session":"parent","text":"hi"}`); !strings.Contains(e, "no parent") {
		t.Errorf("parent of a session without one: %s", e)
	}
	s.self = func() string { return "" }
	if e := callErr(t, s, "tell_session", `{"session":"#2","text":"hi"}`); !strings.Contains(e, "can't tell which session is calling") {
		t.Errorf("no caller: %s", e)
	}
	// The switch that keeps ccdash from typing into / starting claude.
	s.self = func() string { return "boss" }
	c.attach = "0"
	if e := callErr(t, s, "tell_session", `{"session":"#2","text":"hi"}`); !strings.Contains(e, "OFF") {
		t.Errorf("attach off: %s", e)
	}
	c.attach = "1"
	if len(c.told) != n {
		t.Errorf("a refused message reached the collector: %v", c.told[n:])
	}
	// --read-only leaves it out.
	ro := New(s.st, "test", true)
	ro.self = s.self
	if e := callErr(t, ro, "tell_session", `{"session":"#2","text":"hi"}`); !strings.Contains(e, "--read-only") {
		t.Errorf("read-only: %s", e)
	}

	// my_sessions says which channel reaches each child.
	o := mySessions(t, s, `{}`)
	reach := map[string]string{}
	for _, rows := range [][]childRow{o.NeedsOperator, o.Working, o.Reported, o.Finished} {
		for _, r := range rows {
			reach[r.Ref] = r.Reach
		}
	}
	if reach["#2"] != "SendMessage:kid-task-a1" || reach["#3"] != "tell_session" || len(reach) != 2 {
		t.Errorf("reach = %v", reach)
	}
}

// TestTellSessionOtherHost: a child on another machine is reached through
// the hub's peer route, and my_sessions points at tell_session for it.
func TestTellSessionOtherHost(t *testing.T) {
	s, f, _ := multiHostServer(t, true)
	s.self = func() string { return "lap-1" }
	call(t, s, "adopt_session", `{"session":"B1"}`)

	out := call(t, s, "tell_session", `{"session":"B1","text":"k8s の件、進めてください"}`)
	if len(f.told) != 1 {
		t.Fatalf("messages through the hub: %v", f.told)
	}
	got := f.told[0]
	if got["to"] != "srv-1" || got["from_session"] != "lap-1" || got["from_ref"] != "A1" || got["text"] != "k8s の件、進めてください" {
		t.Errorf("handed to B: %v", got)
	}
	if _, ok := got["from_host"]; ok {
		t.Errorf("from_host sent (%v): the hub names the device", got["from_host"])
	}
	if !strings.Contains(out, `"result": "queued"`) || !strings.Contains(out, `"to": "B1"`) || !strings.Contains(out, "mid-turn") || strings.Contains(out, `"peer"`) {
		t.Errorf("result: %s", out)
	}
	o := mySessions(t, s, `{}`)
	rows := append(append(append(o.NeedsOperator, o.Working...), o.Reported...), o.Finished...)
	if len(rows) != 1 || rows[0].Ref != "B1" || rows[0].Reach != "tell_session" || rows[0].Peer != "" {
		t.Errorf("my_sessions rows: %+v", rows)
	}
	// Not the caller's child there: refused here.
	if e := callErr(t, s, "tell_session", `{"session":"B2","text":"hi"}`); !strings.Contains(e, "neither your child nor your parent") {
		t.Errorf("stranger on B: %s", e)
	}
	// B's attach switch.
	f.attach = false
	if e := callErr(t, s, "tell_session", `{"session":"B1","text":"hi"}`); !strings.Contains(e, "OFF on host server") {
		t.Errorf("attach off on B: %s", e)
	}
	if len(f.told) != 1 {
		t.Errorf("refused messages reached B: %v", f.told)
	}
}
