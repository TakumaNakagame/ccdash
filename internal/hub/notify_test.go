package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// fakeDevice serves the device API the watcher polls; tests mutate it
// between polls.
type fakeDevice struct {
	mu        sync.Mutex
	sessions  string
	approvals string
	ptys      string
	screens   map[string][]string
}

func (f *fakeDevice) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/api/sessions":
		_, _ = w.Write([]byte(f.sessions))
	case r.URL.Path == "/api/approvals":
		_, _ = w.Write([]byte(f.approvals))
	case r.URL.Path == "/pty/":
		_, _ = w.Write([]byte(f.ptys))
	case strings.HasSuffix(r.URL.Path, "/text"):
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/pty/"), "/text")
		_ = json.NewEncoder(w).Encode(map[string]any{"rows": f.screens[key]})
	default:
		http.NotFound(w, r)
	}
}

func TestWatchOnce(t *testing.T) {
	f := &fakeDevice{
		sessions:  `[{"session_id":"s1","num":7,"title":"build the thing","status":"active","first_seen":"2026-10-05T00:00:00Z","last_seen":"2026-10-05T00:00:00Z"}]`,
		approvals: `[]`,
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := &http.Client{Transport: rewrite{srv.URL}}
	d := Device{ID: "dev1", Name: "haruna"}
	st := newWatchState()
	ctx := context.Background()
	set := func(js string) { f.mu.Lock(); f.sessions = js; f.mu.Unlock() }

	if ns, ss, err := watchOnce(ctx, c, d, st); err != nil || len(ns) != 0 || len(ss) != 1 {
		t.Fatalf("baseline poll = %v %d %v, want nothing", ns, len(ss), err)
	}

	// The collector marks a question.
	set(`[{"session_id":"s1","num":7,"title":"build the thing","status":"active","attention":"needs_you","attention_reason":"質問: 好きな季節は？","first_seen":"2026-10-05T00:00:00Z","last_seen":"2026-10-05T00:00:00Z"}]`)
	ns, _, _ := watchOnce(ctx, c, d, st)
	if len(ns) != 1 || !strings.Contains(ns[0].Title, "要対応") || ns[0].URL != "/#/d/dev1/s/s1" || !strings.Contains(ns[0].Body, "#7 build the thing") || !strings.Contains(ns[0].Body, "好きな季節") {
		t.Fatalf("question = %+v", ns)
	}
	if ns, _, _ := watchOnce(ctx, c, d, st); len(ns) != 0 {
		t.Fatalf("repeat notified: %+v", ns)
	}

	// The turn finishes (done), and an approval appears on another session.
	set(`[{"session_id":"s1","num":7,"title":"build the thing","status":"idle","attention":"done","first_seen":"2026-10-05T00:00:00Z","last_seen":"2026-10-05T00:00:00Z"},
	      {"session_id":"s2","num":8,"title":"other","status":"active","attention":"needs_you","attention_reason":"承認待ち","pending_count":1,"first_seen":"2026-10-05T00:00:00Z","last_seen":"2026-10-05T00:00:00Z"}]`)
	f.mu.Lock()
	f.approvals = `[{"id":3,"session_id":"s2","tool":"Bash","status":"pending","tool_input":{},"timestamp":"2026-10-05T00:00:00Z"}]`
	f.mu.Unlock()
	ns, _, _ = watchOnce(ctx, c, d, st)
	got := []string{}
	for _, n := range ns {
		got = append(got, n.Title+"/"+n.Body)
	}
	joined := strings.Join(got, ",")
	if len(ns) != 2 || !strings.Contains(joined, "完了") || !strings.Contains(joined, "承認待ち: Bash") {
		t.Fatalf("done + approval = %v", got)
	}

	// Seen: attention cleared → no notification.
	set(`[{"session_id":"s1","num":7,"title":"build the thing","status":"idle","first_seen":"2026-10-05T00:00:00Z","last_seen":"2026-10-05T00:00:00Z"}]`)
	if ns, _, _ := watchOnce(ctx, c, d, st); len(ns) != 0 {
		t.Fatalf("cleared attention notified: %+v", ns)
	}

	// A fresh spawn stuck on the folder-trust question (no session yet);
	// a spawn that already has a session key is the session's business.
	f.mu.Lock()
	f.ptys = `[{"key":"pid-42","alive":true,"pid":42,"cwd":"/w/market-watch","prompt":"確認: Is this a project you trust?"},
	           {"key":"pid-43","alive":true,"pid":43,"prompt":"確認: x"},{"key":"s9","alive":true,"pid":43}]`
	f.mu.Unlock()
	ns, _, _ = watchOnce(ctx, c, d, st)
	if len(ns) != 1 || ns[0].URL != "/#/d/dev1/p/pid-42" || !strings.Contains(ns[0].Body, "market-watch") || !strings.Contains(ns[0].Body, "trust") {
		t.Fatalf("spawn prompt = %+v", ns)
	}
	if ns, _, _ := watchOnce(ctx, c, d, st); len(ns) != 0 {
		t.Fatalf("spawn prompt repeated: %+v", ns)
	}
}

func TestBoard(t *testing.T) {
	hub := newTestHub(t, true)
	hub.storeSnapshot(Device{ID: "d1", Name: "haruna"}, []model.Session{
		{SessionID: "a", Status: model.StatusActive},
		{SessionID: "b", Status: model.StatusIdle, Attention: model.AttentionNeedsYou},
		{SessionID: "c", Status: model.StatusIdle, Attention: model.AttentionDone},
		{SessionID: "d", Status: model.StatusIdle},
		{SessionID: "e", Status: model.StatusStopped, Unseen: true}, // NEW without a done flag
	}, nil)
	if n := hub.badgeCount(); n != 3 {
		t.Fatalf("badgeCount = %d, want 3 (b, c, e)", n)
	}
	wa := serve(hub.Handler(), "GET", "/api/active", "", nil)
	var act []struct {
		Session model.Session `json:"session"`
	}
	_ = json.Unmarshal(wa.Body.Bytes(), &act)
	// b (needs you) first, then a (working); idle c is alive too; d is idle as well.
	if len(act) != 4 || act[0].Session.SessionID != "b" || act[1].Session.SessionID != "a" {
		t.Fatalf("active = %s", wa.Body)
	}
	w := serve(hub.Handler(), "GET", "/api/board", "", nil)
	var b map[string][]struct {
		DeviceName string        `json:"device_name"`
		Session    model.Session `json:"session"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if len(b["needs_you"]) != 1 || len(b["working"]) != 1 || len(b["done"]) != 2 || b["needs_you"][0].DeviceName != "haruna" {
		t.Fatalf("board = %s", w.Body)
	}
}

// rewrite sends the watcher's http://device/... requests to the test server.
type rewrite struct{ base string }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	target, _ := http.NewRequest(req.Method, r.base+u.RequestURI(), req.Body)
	return http.DefaultTransport.RoundTrip(target)
}

func TestPushSubscribeAndKey(t *testing.T) {
	hub := newTestHub(t, true)
	h := hub.Handler()
	w := serve(h, "GET", "/api/push/key", "", nil)
	var k struct{ PublicKey string }
	_ = json.Unmarshal(w.Body.Bytes(), &k)
	if w.Code != http.StatusOK || len(k.PublicKey) < 60 {
		t.Fatalf("push key = %d %s", w.Code, w.Body)
	}
	if w2 := serve(h, "GET", "/api/push/key", "", nil); !strings.Contains(w2.Body.String(), k.PublicKey) {
		t.Error("VAPID key changed between calls")
	}
	sub := `{"endpoint":"https://push.example/abc","keys":{"p256dh":"BAAA","auth":"x"}}`
	if w := serve(h, "POST", "/api/push/subscribe", sub, map[string]string{"Content-Type": "application/json"}); w.Code != http.StatusNoContent {
		t.Fatalf("subscribe = %d %s", w.Code, w.Body)
	}
	if w := serve(h, "POST", "/api/push/subscribe", `{"endpoint":"http://insecure/x","keys":{"p256dh":"a","auth":"b"}}`, nil); w.Code != http.StatusBadRequest {
		t.Errorf("http endpoint accepted: %d", w.Code)
	}
	subs, _ := hub.st.listSubs(context.Background())
	if len(subs) != 1 {
		t.Fatalf("subs = %d", len(subs))
	}
	if w := serve(h, "POST", "/api/push/unsubscribe", `{"endpoint":"https://push.example/abc"}`, nil); w.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe = %d", w.Code)
	}
}

// TestSessionLink: /s/<id> lands on that session's chat on the device that
// reports it, asks for a login first, and says so when nobody has it.
func TestSessionLink(t *testing.T) {
	hub := newTestHub(t, true)
	hub.storeSnapshot(Device{ID: "d1"}, []model.Session{{SessionID: "x"}}, nil)
	hub.storeSnapshot(Device{ID: "d2"}, []model.Session{{SessionID: "abc-123"}}, nil)
	w := serve(hub.Handler(), "GET", "/s/abc-123", "", nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/#/d/d2/s/abc-123" {
		t.Fatalf("link = %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := serve(hub.Handler(), "GET", "/api/locate/abc-123", "", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"device_id":"d2"`) {
		t.Fatalf("locate = %d %s", w.Code, w.Body)
	}
	if w := serve(hub.Handler(), "GET", "/s/nope", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown session = %d", w.Code)
	}
	w = serve(newTestHub(t, false).Handler(), "GET", "/s/abc-123", "", nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/auth/login?next=%2Fs%2Fabc-123" {
		t.Fatalf("logged out = %d %q", w.Code, w.Header().Get("Location"))
	}
}

// TestRestartNotice: a device scheduling a restart pushes once (not on the
// first look), an expired schedule pushes, a cancel doesn't.
func TestRestartNotice(t *testing.T) {
	d := Device{ID: "dev1", Name: "desk"}
	st := newWatchState()
	pending := &model.RestartState{Pending: true, Hosted: 3, Waiting: []model.RestartBlocker{{SessionID: "a", Why: "作業中"}}}
	if _, ok := restartNotice(d, pending, st); ok {
		t.Error("baseline pushed")
	}
	if _, ok := restartNotice(d, nil, st); ok {
		t.Error("no state pushed")
	}
	n, ok := restartNotice(d, pending, st)
	if !ok || !strings.Contains(n.Title, "再起動を予約") || !strings.Contains(n.Body, "1 件待ち") || !strings.Contains(n.Body, "3 件") {
		t.Errorf("pending: %v %+v", ok, n)
	}
	if _, ok := restartNotice(d, pending, st); ok {
		t.Error("repeat pushed")
	}
	at := time.Now()
	if n, ok := restartNotice(d, &model.RestartState{Last: "expired", LastAt: at}, st); !ok || !strings.Contains(n.Title, "時間切れ") {
		t.Errorf("expired: %v %+v", ok, n)
	}
	restartNotice(d, pending, st)
	if _, ok := restartNotice(d, &model.RestartState{Last: "cancelled", LastAt: at}, st); ok {
		t.Error("cancel pushed")
	}
}
