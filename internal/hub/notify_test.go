package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/takumanakagame/ccmanage/internal/model"
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
}

func TestBoard(t *testing.T) {
	hub := newTestHub(t, true)
	hub.storeSnapshot(Device{ID: "d1", Name: "haruna"}, []model.Session{
		{SessionID: "a", Status: model.StatusActive},
		{SessionID: "b", Status: model.StatusIdle, Attention: model.AttentionNeedsYou},
		{SessionID: "c", Status: model.StatusIdle, Attention: model.AttentionDone},
		{SessionID: "d", Status: model.StatusIdle},
	})
	w := serve(hub.Handler(), "GET", "/api/board", "", nil)
	var b map[string][]struct {
		DeviceName string        `json:"device_name"`
		Session    model.Session `json:"session"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if len(b["needs_you"]) != 1 || len(b["working"]) != 1 || len(b["done"]) != 1 || b["done"][0].Session.SessionID != "c" || b["needs_you"][0].DeviceName != "haruna" {
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
