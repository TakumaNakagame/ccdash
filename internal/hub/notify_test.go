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
)

func TestScreenPromptKind(t *testing.T) {
	ask := []string{
		"←  ☐ 果物  ☐ 色  ✔ Submit  →", "", "好きな果物はどれですか？", "",
		"❯ 1. りんご", "     りんご", "  2. みかん", "  3. Type something.", "──────", "  4. Chat about this", "",
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
	}
	if k, q := screenPromptKind(ask); k != "question" || q != "好きな果物はどれですか？" {
		t.Errorf("ask dialog = %q %q", k, q)
	}
	perm := []string{"╭────╮", "│ Bash command │", "│ rm -rf build │", "│ Do you want to proceed? │", "│ ❯ 1. Yes │", "│   2. No │", "╰────╯"}
	if k, q := screenPromptKind(perm); k != "confirm" || q != "Do you want to proceed?" {
		t.Errorf("permission dialog = %q %q", k, q)
	}
	trust := []string{" Quick safety check: Is this a project you created or one you trust?", "", " ❯ No, exit", "   Yes, I trust this folder", "", " Enter to confirm · Esc to cancel"}
	if k, q := screenPromptKind(trust); k != "confirm" || !strings.Contains(q, "trust") {
		t.Errorf("unnumbered trust menu = %q %q", k, q)
	}
	// A wrapped past prompt above the (empty) input box is not a menu.
	scrollback := []string{"❯ テストです。何も調べず、AskUserQuestion を使って", "  1問目「好きな果物」を聞いて", "● はい", "────", "❯ ", "────"}
	if k, _ := screenPromptKind(scrollback); k != "" {
		t.Errorf("past prompt detected as %q", k)
	}
	idle := []string{"● done", "────", "❯ ", "────", "  ⏵⏵ auto mode on"}
	if k, _ := screenPromptKind(idle); k != "" {
		t.Errorf("idle input prompt detected as %q", k)
	}
}

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
		ptys:      `[{"key":"pid-10","alive":true,"pid":10},{"key":"s1","alive":true,"pid":10}]`,
		screens:   map[string][]string{"s1": {"❯ "}},
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := &http.Client{Transport: rewrite{srv.URL}}
	d := Device{ID: "dev1", Name: "haruna"}
	st := newWatchState()
	ctx := context.Background()

	if ns, err := watchOnce(ctx, c, d, st); err != nil || len(ns) != 0 {
		t.Fatalf("baseline poll = %v %v, want nothing", ns, err)
	}

	// A question appears on the hosted claude's screen.
	f.mu.Lock()
	f.screens["s1"] = []string{"☐ 季節", "好きな季節は？", "❯ 1. 春", "  2. 夏", "  3. Type something.", "  4. Chat about this", "Enter to select"}
	f.mu.Unlock()
	ns, _ := watchOnce(ctx, c, d, st)
	if len(ns) != 1 || !strings.Contains(ns[0].Title, "質問") || ns[0].URL != "/#/d/dev1/s/s1" || !strings.Contains(ns[0].Body, "#7 build the thing") {
		t.Fatalf("question = %+v", ns)
	}
	// Same question again: no repeat.
	if ns, _ := watchOnce(ctx, c, d, st); len(ns) != 0 {
		t.Fatalf("repeat question notified: %+v", ns)
	}

	// A session that ran a whole short turn between two polls (never seen
	// active) still counts as finished.
	f.mu.Lock()
	f.sessions = strings.Replace(f.sessions, `]`, `,{"session_id":"s3","num":9,"title":"quick","status":"idle","first_seen":"2026-10-05T00:00:00Z","last_seen":"`+time.Now().UTC().Format(time.RFC3339)+`"}]`, 1)
	f.mu.Unlock()
	if ns, _ := watchOnce(ctx, c, d, st); len(ns) != 1 || !strings.Contains(ns[0].Title, "完了") || !strings.Contains(ns[0].Body, "#9") {
		t.Fatalf("short turn = %+v", ns)
	}
	if ns, _ := watchOnce(ctx, c, d, st); len(ns) != 0 {
		t.Fatalf("idle session re-notified: %+v", ns)
	}

	// The turn ends and an approval for another session arrives.
	f.mu.Lock()
	f.screens["s1"] = []string{"❯ "}
	f.sessions = strings.Replace(f.sessions, `"active"`, `"idle"`, 1)
	f.approvals = `[{"id":3,"session_id":"s2","tool":"Bash","status":"pending","tool_input":{},"timestamp":"2026-10-05T00:00:00Z"}]`
	f.mu.Unlock()
	ns, _ = watchOnce(ctx, c, d, st)
	titles := []string{}
	for _, n := range ns {
		titles = append(titles, n.Title)
	}
	if len(ns) != 2 || !strings.Contains(strings.Join(titles, ","), "完了") || !strings.Contains(strings.Join(titles, ","), "承認待ち") {
		t.Fatalf("done + approval = %v", titles)
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
