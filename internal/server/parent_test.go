package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// TestPTYStartParentAndBrief: a spawn carries its parent to the session's
// row (once), shows it in GET /pty before there is a row, and its prompt is
// kept as a brief — redacted, and tied to the session id when the first
// hook names it.
func TestPTYStartParentAndBrief(t *testing.T) {
	old := newPTYCommand
	newPTYCommand = func(args ...string) *exec.Cmd { return exec.Command("sh", "-c", "cat") }
	t.Cleanup(func() { newPTYCommand = old })
	s, d, tok := newTestServer(t)
	ctx := context.Background()

	secret := "ghp_" + strings.Repeat("a1B2", 9)
	body := fmt.Sprintf(`{"cwd":"/tmp","prompt":"fix the login page, token %s","project":"web","parent":"boss-1","parentHost":"dev-9"}`, secret)
	r := do(t, s, "POST", "/pty/start", tok, []byte(body))
	if r.status != http.StatusOK {
		t.Fatalf("start = %d %s", r.status, r.body)
	}
	var started struct {
		PtyKey string
		Tag    int
	}
	_ = json.Unmarshal(r.body, &started)
	t.Cleanup(func() { do(t, s, "DELETE", "/pty/"+started.PtyKey, tok, nil) })

	var rows []ptyInfo
	_ = json.Unmarshal(do(t, s, "GET", "/pty/", tok, nil).body, &rows)
	if len(rows) != 1 || rows[0].Parent != "boss-1" || rows[0].Key != started.PtyKey {
		t.Fatalf("GET /pty = %+v", rows)
	}
	if bs, _ := d.SessionBriefs(ctx, "kid-1"); len(bs) != 0 {
		t.Fatalf("brief linked before the session is known: %+v", bs)
	}

	// The child's first hook names the session: row, alias, parent, project
	// and brief all land at once.
	req, _ := http.NewRequest("POST", "/hooks/session-start", strings.NewReader(`{"session_id":"kid-1","cwd":"/tmp"}`))
	req.Header.Set("X-Ccdash-Token", tok)
	req.Header.Set("X-Ccdash-Wrapper-Pid", fmt.Sprint(started.Tag))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hook = %d %s", rec.Code, rec.Body)
	}
	x, ok, _ := d.GetSession(ctx, "kid-1")
	if !ok || x.Parent != "boss-1" || x.ParentHost != "dev-9" || x.Project != "web" {
		t.Fatalf("row = %+v", x)
	}
	r = do(t, s, "GET", "/api/sessions/kid-1/brief", tok, nil)
	var bs []model.Brief
	_ = json.Unmarshal(r.body, &bs)
	if r.status != 200 || len(bs) != 1 {
		t.Fatalf("brief = %d %s", r.status, r.body)
	}
	if b := bs[0]; !strings.HasPrefix(b.Text, "fix the login page") || strings.Contains(b.Text, secret) || b.Parent != "boss-1" || b.Project != "web" || b.Cwd != "/tmp" {
		t.Errorf("brief = %+v", b)
	}

	// A later spawn-time parent never moves it; the edit route does.
	_ = d.SetParent(ctx, "kid-1", "someone-else", "")
	if x, _, _ := d.GetSession(ctx, "kid-1"); x.Parent != "boss-1" {
		t.Errorf("spawn-time parent overwritten: %q", x.Parent)
	}
	if r := do(t, s, "POST", "/api/sessions/kid-1/parent", tok, []byte(`{"parent":"boss-2"}`)); r.status != 200 {
		t.Fatalf("adopt = %d %s", r.status, r.body)
	}
	if x, _, _ := d.GetSession(ctx, "kid-1"); x.Parent != "boss-2" || x.ParentHost != "" {
		t.Errorf("after adopt: %q %q", x.Parent, x.ParentHost)
	}
	for _, bad := range []string{`{"parent":"kid-1"}`, `{"parent":"a b"}`, `{"parent":"x","parentHost":"../y"}`} {
		if r := do(t, s, "POST", "/api/sessions/kid-1/parent", tok, []byte(bad)); r.status != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.status)
		}
	}
	if r := do(t, s, "POST", "/api/sessions/kid-1/parent", tok, []byte(`{"parent":""}`)); r.status != 200 {
		t.Fatalf("release = %d", r.status)
	}
	if x, _, _ := d.GetSession(ctx, "kid-1"); x.Parent != "" {
		t.Errorf("after release: %q", x.Parent)
	}
	if r := do(t, s, "POST", "/api/sessions/boss-1/children-seen", tok, []byte(`{"at":1700000000000}`)); r.status != 200 {
		t.Errorf("children-seen = %d", r.status)
	}

	// A resume's message is not a brief; neither is a spawn with no prompt.
	for _, b := range []string{`{"cwd":"/tmp","sessionId":"old-1","resumeId":"old-1","prompt":"carry on"}`, `{"cwd":"/tmp"}`} {
		r := do(t, s, "POST", "/pty/start", tok, []byte(b))
		var st struct{ PtyKey string }
		_ = json.Unmarshal(r.body, &st)
		t.Cleanup(func() { do(t, s, "DELETE", "/pty/"+st.PtyKey, tok, nil) })
	}
	if bs, _ := d.SessionBriefs(ctx, "old-1"); len(bs) != 0 {
		t.Errorf("resume kept a brief: %+v", bs)
	}
	if r := do(t, s, "POST", "/pty/start", tok, []byte(`{"cwd":"/tmp","parent":"no good"}`)); r.status != http.StatusBadRequest {
		t.Errorf("bad parent = %d, want 400", r.status)
	}
}
