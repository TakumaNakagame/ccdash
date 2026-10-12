package hub

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/takumanakagame/ccmanage/internal/tunnel"
)

func newLocalHub(t *testing.T) (*Hub, string) {
	t.Helper()
	dir := t.TempDir()
	login := filepath.Join(dir, "web-login")
	h, err := New(context.Background(), Config{
		Listen:    "127.0.0.1:9124",
		DataDir:   filepath.Join(dir, "web"),
		PublicURL: "http://127.0.0.1:9124",
		Local:     &LocalConfig{LoginFile: login, User: "takuma"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, login
}

func loginToken(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("token")
}

func local(hdr map[string]string) map[string]string {
	out := map[string]string{"Host": "127.0.0.1:9124"}
	for k, v := range hdr {
		out[k] = v
	}
	return out
}

func TestLocalConfigChecks(t *testing.T) {
	dir := t.TempDir()
	lc := &LocalConfig{LoginFile: filepath.Join(dir, "l"), User: "u"}
	for _, c := range []Config{
		{Listen: "0.0.0.0:9124", PublicURL: "http://127.0.0.1:9124"},
		{Listen: "127.0.0.1:9124", PublicURL: "https://ccdash.example"},
		{Listen: "127.0.0.1:9124", PublicURL: "http://127.0.0.1:9124", ReadTokens: []string{strings.Repeat("x", 40)}},
	} {
		c.DataDir, c.Local = dir, lc
		if h, err := New(context.Background(), c); err == nil {
			_ = h.Close()
			t.Errorf("New(%+v) accepted", c)
		}
	}
}

func TestLocalLogin(t *testing.T) {
	h, loginFile := newLocalHub(t)
	handler := h.Handler()

	if fi, err := os.Stat(loginFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("login file: %v %v", fi, err)
	}
	if got := strings.TrimSpace(readFile(t, loginFile)); got != h.LoginURL() || !strings.HasPrefix(got, "http://127.0.0.1:9124/auth/local?token=") {
		t.Fatalf("login file = %q, LoginURL = %q", got, h.LoginURL())
	}

	// No cookie: the page explains how to log in, the API says 401.
	if w := serve(handler, "GET", "/", "", local(nil)); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "ccdash web open") {
		t.Errorf("page without cookie = %d %s", w.Code, w.Body)
	}
	if w := serve(handler, "GET", "/api/me", "", local(nil)); w.Code != http.StatusUnauthorized {
		t.Errorf("api without cookie = %d", w.Code)
	}
	if w := serve(handler, "GET", "/auth/local?token=nope", "", local(nil)); w.Code != http.StatusForbidden {
		t.Errorf("wrong token = %d", w.Code)
	}

	tok := loginToken(t, loginFile)
	w := serve(handler, "GET", "/auth/local?token="+tok+"&next=/%23/grid", "", local(nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `content="0;url=/#/grid"`) {
		t.Fatalf("login = %d %s", w.Code, w.Body)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == localCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge < 86400 {
		t.Fatalf("cookie = %+v", cookie)
	}
	// One use: the file now holds a fresh token and the old one is dead.
	if next := loginToken(t, loginFile); next == tok || next == "" {
		t.Errorf("token not rotated after use")
	}
	if w := serve(handler, "GET", "/auth/local?token="+tok, "", local(nil)); w.Code != http.StatusForbidden {
		t.Errorf("reused token = %d, want 403", w.Code)
	}

	withCookie := local(map[string]string{"Cookie": localCookie + "=" + cookie.Value})
	w = serve(handler, "GET", "/api/me", "", withCookie)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"local":true`) || !strings.Contains(w.Body.String(), `"email":"takuma"`) {
		t.Fatalf("me = %d %s", w.Code, w.Body)
	}
	if w := serve(handler, "GET", "/", "", withCookie); w.Code != http.StatusOK {
		t.Errorf("page with cookie = %d", w.Code)
	}
	// A hub cookie from a dev hub on another port is not ours.
	if w := serve(handler, "GET", "/api/me", "", local(map[string]string{"Cookie": sessionCookie + "=" + cookie.Value})); w.Code != http.StatusUnauthorized {
		t.Errorf("hub cookie name accepted: %d", w.Code)
	}
	// DNS rebinding: right cookie (a browser would not send it, but still)
	// and a foreign Host.
	if w := serve(handler, "GET", "/api/me", "", map[string]string{"Host": "evil.example:9124", "Cookie": localCookie + "=" + cookie.Value}); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("foreign host = %d, want 421", w.Code)
	}
	// Device management, OIDC and the tunnel endpoint don't exist here.
	for _, p := range [][2]string{{"POST", "/api/devices"}, {"POST", "/api/devices/local/rotate"}, {"DELETE", "/api/devices/local"}, {"GET", tunnel.Path}, {"GET", "/auth/callback"}, {"GET", "/auth/logout"}} {
		if w := serve(handler, p[0], p[1], `{"name":"x"}`, withCookie); w.Code == http.StatusOK || w.Code == http.StatusNoContent || w.Code == http.StatusSwitchingProtocols {
			t.Errorf("%s %s = %d on the local portal", p[0], p[1], w.Code)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestServeLocal: the in-process link carries the portal's device
// requests (the same proxy hub mode uses) and the device list shows the
// one machine online.
func TestServeLocal(t *testing.T) {
	h, loginFile := newLocalHub(t)
	handler := h.Handler()
	w := serve(handler, "GET", "/auth/local?token="+loginToken(t, loginFile), "", local(nil))
	cookie := w.Result().Cookies()[0]
	withCookie := local(map[string]string{"Cookie": localCookie + "=" + cookie.Value})

	hubEnd, devEnd := net.Pipe()
	devSess, err := yamux.Server(devEnd, tunnel.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = (&http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "device saw "+r.Method+" "+r.URL.Path+" token="+r.Header.Get("X-Ccdash-Token"))
		})}).Serve(devSess)
	}()
	hubSess, err := yamux.Client(hubEnd, tunnel.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.ServeLocal(hubSess, "work-mac", "v1.2.3") }()

	deadline := time.Now().Add(3 * time.Second)
	for h.conn(LocalDeviceID) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	w = serve(handler, "GET", "/api/devices", "", withCookie)
	if !strings.Contains(w.Body.String(), `"id":"local"`) || !strings.Contains(w.Body.String(), `"name":"work-mac"`) || !strings.Contains(w.Body.String(), `"online":true`) {
		t.Fatalf("devices = %s", w.Body)
	}
	// Through the real network stack: the proxy uses the request context.
	srv := &http.Server{Handler: handler}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/api/d/local/api/sessions", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "device saw GET /api/sessions token=" {
		t.Fatalf("proxied = %d %q", resp.StatusCode, b)
	}

	_ = hubSess.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeLocal did not return after the link closed")
	}
	if h.conn(LocalDeviceID) != nil {
		t.Error("device still online after the link closed")
	}
}

func TestPushSubject(t *testing.T) {
	h, _ := newLocalHub(t)
	if got := h.pushSubject(); !strings.HasPrefix(got, "https://") {
		t.Errorf("local push subject = %q, want an https contact", got)
	}
	hub := newTestHub(t, false)
	if got := hub.pushSubject(); got != "https://hub.example" {
		t.Errorf("hub push subject = %q", got)
	}
}

type capturePush struct{ reqs []*http.Request }

func (c *capturePush) Do(r *http.Request) (*http.Response, error) {
	c.reqs = append(c.reqs, r)
	return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// TestLocalPush: the local portal sends ordinary encrypted Web Push with a
// VAPID token whose contact is an https URL (not its loopback address).
func TestLocalPush(t *testing.T) {
	h, _ := newLocalHub(t)
	cp := &capturePush{}
	h.pushClient = cp
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	b64 := base64.RawURLEncoding.EncodeToString
	ctx := context.Background()
	if err := h.st.addSub(ctx, "takuma", pushSub{Endpoint: "https://fcm.googleapis.com/fcm/send/abc", P256dh: b64(priv.PublicKey().Bytes()), Auth: b64(auth)}); err != nil {
		t.Fatal(err)
	}
	h.push(ctx, notification{Title: "work-mac · 要対応", Body: "Bash", URL: "/#/d/local/s/x", Tag: "local:x"})
	if len(cp.reqs) != 1 {
		t.Fatalf("%d push requests, want 1", len(cp.reqs))
	}
	r := cp.reqs[0]
	if r.URL.String() != "https://fcm.googleapis.com/fcm/send/abc" || r.Header.Get("Content-Encoding") != "aes128gcm" {
		t.Errorf("push request %s, encoding %q", r.URL, r.Header.Get("Content-Encoding"))
	}
	body, _ := io.ReadAll(r.Body)
	if strings.Contains(string(body), "要対応") {
		t.Error("payload sent in the clear")
	}
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "vapid t=")
	if !ok {
		t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
	}
	jwt, _, _ = strings.Cut(jwt, ",")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt = %q", jwt)
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct{ Aud, Sub string }
	if err := json.Unmarshal(claims, &c); err != nil {
		t.Fatal(err)
	}
	if c.Aud != "https://fcm.googleapis.com" || c.Sub != "https://github.com/takumanakagame/ccmanage" {
		t.Errorf("vapid claims = %+v", c)
	}
}

type pushResult struct {
	status int
	err    error
}

func (p pushResult) Do(*http.Request) (*http.Response, error) {
	if p.err != nil {
		return nil, p.err
	}
	return &http.Response{StatusCode: p.status, Body: io.NopCloser(strings.NewReader("why"))}, nil
}

// TestPushTestDiagnosis: the portal's notification test sends to one
// browser and gets back what the push service did.
func TestPushTestDiagnosis(t *testing.T) {
	h, loginFile := newLocalHub(t)
	handler := h.Handler()
	w := serve(handler, "GET", "/auth/local?token="+loginToken(t, loginFile), "", local(nil))
	cookie := local(map[string]string{"Cookie": localCookie + "=" + w.Result().Cookies()[0].Value, "Content-Type": "application/json"})

	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	b64 := base64.RawURLEncoding.EncodeToString
	const ep = "https://fcm.googleapis.com/fcm/send/abc"
	sub := `{"endpoint":"` + ep + `","keys":{"p256dh":"` + b64(priv.PublicKey().Bytes()) + `","auth":"` + b64(auth) + `"}}`
	if w := serve(handler, "POST", "/api/push/subscribe", sub, cookie); w.Code != http.StatusNoContent {
		t.Fatalf("subscribe = %d %s", w.Code, w.Body)
	}
	try := func(c pushResult) map[string]any {
		t.Helper()
		h.pushClient = c
		w := serve(handler, "POST", "/api/push/test", `{"endpoint":"`+ep+`"}`, cookie)
		if w.Code != http.StatusOK {
			t.Fatalf("test = %d %s", w.Code, w.Body)
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	if out := try(pushResult{status: http.StatusCreated}); out["ok"] != true || out["status"] != float64(201) || out["service"] != "fcm.googleapis.com" {
		t.Errorf("accepted: %v", out)
	}
	if out := try(pushResult{status: http.StatusForbidden}); out["ok"] != false || out["status"] != float64(403) || !strings.Contains(out["error"].(string), "403") {
		t.Errorf("rejected: %v", out)
	}
	if out := try(pushResult{err: errors.New("proxyconnect tcp: dial tcp: i/o timeout")}); out["ok"] != false || out["status"] != float64(0) || !strings.Contains(out["error"].(string), "i/o timeout") {
		t.Errorf("unreachable: %v", out)
	}
	// Gone: dropped, and a second test says it isn't registered.
	if out := try(pushResult{status: http.StatusGone}); out["ok"] != false {
		t.Errorf("gone: %v", out)
	}
	if w := serve(handler, "POST", "/api/push/test", `{"endpoint":"`+ep+`"}`, cookie); w.Code != http.StatusNotFound {
		t.Errorf("after gone = %d, want 404", w.Code)
	}
}
