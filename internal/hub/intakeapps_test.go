package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const intakeTok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func tokHash(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}

// intakeDevice is a fake collector that records uploads and spawns.
type intakeDev struct {
	mu      sync.Mutex
	uploads []string // content types
	notes   string
	start   map[string]any
}

func newIntakeHub(t *testing.T, action string) (*Hub, *intakeDev, string) {
	t.Helper()
	h, err := New(context.Background(), Config{
		Listen: "127.0.0.1:0", DataDir: t.TempDir(), PublicURL: "https://hub.example",
		Auth: AuthConfig{Issuer: "https://issuer.example", ClientID: "id", ClientSecret: "s", AllowedEmails: []string{"dev@localhost"}, NoAuth: true},
		IntakeApps: []IntakeApp{{Name: "shop", TokenSHA256: tokHash(intakeTok), Owner: "Dev@Localhost", Host: "box", Cwd: "/src/shop", Project: "Shop", Mode: "auto", Action: action}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	d, _, err := h.st.createDevice(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	f := &intakeDev{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/hub/upload":
			b, _ := io.ReadAll(r.Body)
			ct := r.Header.Get("Content-Type")
			f.uploads = append(f.uploads, ct)
			if ct == "text/markdown" {
				f.notes = string(b)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"path": "/up/" + r.Header.Get("X-Filename"), "size": len(b)})
		case "/pty/start":
			_ = json.NewDecoder(r.Body).Decode(&f.start)
			_, _ = w.Write([]byte(`{"ptyKey":"pid-42"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	h.mu.Lock()
	h.conns[d.ID] = &deviceConn{since: time.Now(), transport: tr, rt: tr, proxy: newDeviceProxy(tr)}
	h.mu.Unlock()
	return h, f, d.ID
}

func intakeForm(t *testing.T, fields map[string]string, images int) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for i := 0; i < images; i++ {
		hd := textproto.MIMEHeader{}
		hd.Set("Content-Disposition", `form-data; name="files"; filename="shot.png"`)
		hd.Set("Content-Type", "image/png")
		pw, _ := mw.CreatePart(hd)
		_, _ = pw.Write([]byte("\x89PNG fake"))
	}
	_ = mw.Close()
	return &b, mw.FormDataContentType()
}

func postIntake(h http.Handler, tok string, body *bytes.Buffer, ct string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/intake/requests", body)
	r.Host = "hub.example"
	r.Header.Set("Content-Type", ct)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestIntakeDraft(t *testing.T) {
	h, f, dev := newIntakeHub(t, "")
	hd := h.Handler()
	body, ct := intakeForm(t, map[string]string{"text": "並べ替えを足して", "url": "https://shop/list", "title": "List", "context": `{"view":"list"}`, "errors": "TypeError: x"}, 2)
	w := postIntake(hd, intakeTok, body, ct)
	if w.Code != http.StatusCreated {
		t.Fatalf("post = %d %s", w.Code, w.Body)
	}
	var res map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res["status"] != "draft" || !strings.HasPrefix(res["url"], "https://hub.example/#/intake?draft=") {
		t.Fatalf("answer = %v", res)
	}
	if len(f.uploads) != 3 || !strings.Contains(f.notes, `{"view":"list"}`) || !strings.Contains(f.notes, "TypeError: x") {
		t.Fatalf("uploads = %v notes = %q", f.uploads, f.notes)
	}
	if f.start != nil {
		t.Fatal("a draft started a session")
	}
	// The owner sees it (owner matched case-insensitively), then removes it.
	g := serve(hd, "GET", "/api/intake/drafts", "", nil)
	var ds []intakeDraft
	if err := json.Unmarshal(g.Body.Bytes(), &ds); err != nil || len(ds) != 1 {
		t.Fatalf("drafts = %s", g.Body)
	}
	d := ds[0]
	if d.ID != res["id"] || d.Host != dev || d.Cwd != "/src/shop" || d.Mode != "auto" || len(d.Paths) != 2 || d.Notes == "" || d.Text != "並べ替えを足して" {
		t.Fatalf("draft = %+v", d)
	}
	if w := serve(hd, "DELETE", "/api/intake/drafts/"+d.ID, "", map[string]string{"Sec-Fetch-Site": "same-origin"}); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	if g := serve(hd, "GET", "/api/intake/drafts", "", nil); strings.TrimSpace(g.Body.String()) != "[]" {
		t.Fatalf("after delete = %s", g.Body)
	}
}

func TestIntakeSession(t *testing.T) {
	h, f, dev := newIntakeHub(t, "session")
	hd := h.Handler()
	if w := serve(hd, "PUT", "/api/intake", `{"routes":[],"footer":"- 応答は日本語"}`, map[string]string{"Sec-Fetch-Site": "same-origin"}); w.Code != http.StatusOK {
		t.Fatalf("footer = %d %s", w.Code, w.Body)
	}
	body, ct := intakeForm(t, map[string]string{"text": "直して", "url": "https://shop/x", "sel": "a\nb"}, 1)
	w := postIntake(hd, intakeTok, body, ct)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"status":"started"`) || !strings.Contains(w.Body.String(), "/#/d/"+dev+"/p/pid-42") {
		t.Fatalf("post = %d %s", w.Code, w.Body)
	}
	p, _ := f.start["prompt"].(string)
	for _, want := range []string{"https://shop/x", "> a\n> b", "/up/shot.png", "直して", "- 応答は日本語", "shop"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if f.start["cwd"] != "/src/shop" || f.start["permissionMode"] != "auto" || f.start["project"] != "Shop" {
		t.Errorf("start = %v", f.start)
	}
}

func TestIntakeRefusals(t *testing.T) {
	h, _, _ := newIntakeHub(t, "")
	hd := h.Handler()
	body, ct := intakeForm(t, map[string]string{"text": "x"}, 0)
	if w := postIntake(hd, "", body, ct); w.Code != http.StatusUnauthorized {
		t.Errorf("no token = %d", w.Code)
	}
	body, ct = intakeForm(t, map[string]string{"text": "x"}, 0)
	if w := postIntake(hd, strings.Repeat("f", 64), body, ct); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d", w.Code)
	}
	body, ct = intakeForm(t, map[string]string{"url": "https://x"}, 0)
	if w := postIntake(hd, intakeTok, body, ct); w.Code != http.StatusBadRequest {
		t.Errorf("no text = %d", w.Code)
	}
	body, ct = intakeForm(t, map[string]string{"text": "x"}, 6)
	if w := postIntake(hd, intakeTok, body, ct); w.Code != http.StatusBadRequest {
		t.Errorf("6 files = %d", w.Code)
	}
	// The caller can't steer where it runs: unknown fields are just ignored.
	body, ct = intakeForm(t, map[string]string{"text": "x", "cwd": "/etc", "mode": "bypassPermissions"}, 0)
	if w := postIntake(hd, intakeTok, body, ct); w.Code != http.StatusCreated {
		t.Errorf("extra fields = %d %s", w.Code, w.Body)
	}
	ds, _ := h.loadIntakeDrafts(context.Background(), "dev@localhost")
	if len(ds) != 1 || ds[0].Cwd != "/src/shop" || ds[0].Mode != "auto" {
		t.Errorf("draft = %+v", ds)
	}
	// Rate limit.
	h.intakeRate["shop"] = make([]time.Time, intakeRatePerHour)
	for i := range h.intakeRate["shop"] {
		h.intakeRate["shop"][i] = time.Now()
	}
	body, ct = intakeForm(t, map[string]string{"text": "x"}, 0)
	if w := postIntake(hd, intakeTok, body, ct); w.Code != http.StatusTooManyRequests {
		t.Errorf("rate = %d", w.Code)
	}
}

func TestIntakeRouteOffWithoutApps(t *testing.T) {
	h := newTestHub(t, true)
	body, ct := intakeForm(t, map[string]string{"text": "x"}, 0)
	if w := postIntake(h.Handler(), intakeTok, body, ct); w.Code == http.StatusCreated {
		t.Fatalf("route answered without apps: %d", w.Code)
	}
}

func TestLoadIntakeApps(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "apps.json")
		_ = os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	ok := `{"apps":[{"name":"rc","token_sha256":"` + strings.ToUpper(tokHash(intakeTok)) + `","owner":"me@example.com","host":"LXC132","cwd":"/src/rc"}]}`
	apps, err := LoadIntakeApps(write(ok))
	if err != nil || len(apps) != 1 || apps[0].Action != "draft" || apps[0].TokenSHA256 != tokHash(intakeTok) {
		t.Fatalf("load = %+v %v", apps, err)
	}
	for _, bad := range []string{
		`{"apps":[{"name":"rc","token_sha256":"abc","owner":"me@x","host":"h","cwd":"/x"}]}`,
		`{"apps":[{"name":"rc","token_sha256":"` + tokHash("a") + `","owner":"me","host":"h","cwd":"/x"}]}`,
		`{"apps":[{"name":"rc","token_sha256":"` + tokHash("a") + `","owner":"me@x","host":"h","cwd":"x"}]}`,
		`{"apps":[{"name":"rc","token_sha256":"` + tokHash("a") + `","owner":"me@x","host":"h","cwd":"/x","action":"run"}]}`,
		`{"apps":[{"name":"rc","token_sha256":"` + tokHash("a") + `","owner":"me@x","host":"h","cwd":"/x","token":"plain"}]}`,
		`{"apps":[{"name":"a","token_sha256":"` + tokHash("a") + `","owner":"me@x","host":"h","cwd":"/x"},{"name":"b","token_sha256":"` + tokHash("a") + `","owner":"me@x","host":"h","cwd":"/x"}]}`,
	} {
		if _, err := LoadIntakeApps(write(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
