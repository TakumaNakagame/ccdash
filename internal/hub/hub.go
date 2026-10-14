// Package hub is the web portal side of ccdash: `ccdash hub serve`.
//
// The hub runs on a home server (in the home lab: home-k8s behind
// proxy02), lists the registered devices, and lets a logged-in operator use
// any connected device's ccdash from a browser. Devices are never dialed:
// each device's collector connects OUT to the hub (internal/tunnel) and the
// hub forwards portal requests through that tunnel to the device's own
// collector API, where an allowlist (internal/server/hub.go) decides what
// the hub may do.
package hub

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/takumanakagame/ccmanage/internal/buildinfo"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/tunnel"
)

//go:embed web
var webFS embed.FS

// Config is everything `ccdash hub serve` needs.
type Config struct {
	Listen    string // e.g. :8080
	DataDir   string // hub.sqlite lives here
	PublicURL string // browser-facing origin, e.g. https://ccdash.g3.lab-dev.net
	Auth      AuthConfig
	// ReadTokens are bearer tokens for machine clients: GET-only access to
	// a fixed set of read routes (see readtoken.go). Empty disables them.
	ReadTokens []string
	// StateSecret keeps the state in this Kubernetes Secret (k8sstore.go)
	// instead of DataDir/hub.sqlite, which is then only imported once
	// when the Secret doesn't exist yet. Required for several replicas.
	StateSecret string
	// Cluster lets replicas forward to each other (cluster.go).
	Cluster ClusterConfig
	k8s     *k8sAPI // tests: a fake API server instead of the in-cluster one
	// Local makes this the local portal (`ccdash web`, local.go): one
	// fixed device linked in-process, token-file login, loopback only.
	Local *LocalConfig
}

// Hub is a running portal.
type Hub struct {
	cfg  Config
	st   stateStore
	auth *authn
	cl   *cluster // nil: a lone hub

	assets string // hash of the embedded web files; the SPA reloads when it changes

	readTokens [][32]byte // SHA-256 of each read token; never the tokens themselves

	pushClient webpush.HTTPClient // nil = a plain http.Client (tests capture pushes)

	mu        sync.Mutex
	conns     map[string]*deviceConn    // device id → live tunnel held by this replica
	refused   map[string]bool           // tailscale logins already logged as refused
	snapshots map[string]deviceSnapshot // device id → latest session list (board)
}

// deviceConn is one connected device's tunnel plus an HTTP client that
// dials through it — or, with peer set, the same through the replica that
// holds the tunnel (cluster.go).
type deviceConn struct {
	sess      *yamux.Session // nil for a peer's tunnel
	open      func() (net.Conn, error)
	peer      string // the replica holding the tunnel; "" = this one
	tokenHash string // the token the tunnel came in with (revoked → dropped)
	since     time.Time
	remote    string
	transport *http.Transport
	proxy     *httputil.ReverseProxy
}

func newDeviceConn(open func() (net.Conn, error), since time.Time, remote string) *deviceConn {
	tr := &http.Transport{
		DialContext:         func(context.Context, string, string) (net.Conn, error) { return open() },
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
	}
	return &deviceConn{open: open, since: since, remote: remote, transport: tr, proxy: newDeviceProxy(tr)}
}

// New opens the hub's store and prepares the handlers.
func New(ctx context.Context, cfg Config) (*Hub, error) {
	switch {
	case cfg.Local != nil:
		if err := cfg.Local.check(cfg); err != nil {
			return nil, err
		}
	case cfg.Auth.NoAuth:
	case cfg.Auth.TailscaleServe:
		// An empty allowlist is allowed here: every visitor gets a 403
		// naming their login, which is how the operator finds the exact
		// string to allow.
	default:
		if len(cfg.Auth.AllowedEmails) == 0 {
			return nil, errNoAllowedEmails
		}
		if cfg.Auth.Issuer == "" || cfg.Auth.ClientID == "" || cfg.Auth.ClientSecret == "" {
			return nil, errors.New("OIDC issuer, client id and client secret are required (or --no-auth for local development)")
		}
	}
	pu, err := url.Parse(cfg.PublicURL)
	if err != nil || pu.Scheme == "" || pu.Host == "" {
		return nil, fmt.Errorf("--public-url must be the browser-facing origin like https://ccdash.example.net, got %q", cfg.PublicURL)
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	readTokens, err := hashReadTokens(cfg.ReadTokens)
	if err != nil {
		return nil, err
	}
	cfg.ReadTokens = nil
	st, err := openState(ctx, cfg)
	if err != nil {
		return nil, err
	}
	key, err := st.secret(ctx, "cookie_key", func() string { return randHex(32) })
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	for i, e := range cfg.Auth.AllowedEmails {
		cfg.Auth.AllowedEmails[i] = strings.ToLower(strings.TrimSpace(e))
	}
	a := &authn{cfg: cfg.Auth, publicURL: cfg.PublicURL, key: []byte(key), secure: pu.Scheme == "https"}
	if cfg.Local != nil {
		if a.local, err = newLocalLogin(cfg.Local, cfg.PublicURL); err != nil {
			_ = st.Close()
			return nil, err
		}
	}
	h := &Hub{
		assets: assetsHash(),
		cfg:    cfg,
		st:     st,
		auth:   a,
		conns:  map[string]*deviceConn{},

		readTokens: readTokens,
	}
	if cfg.Cluster.Peers != "" && cfg.Local == nil {
		ck, err := st.secret(ctx, clusterKeyName, func() string { return randHex(32) })
		if err != nil {
			_ = st.Close()
			return nil, err
		}
		h.cl = newCluster(h, cfg.Cluster, ck)
	}
	if ks, ok := st.(*k8sStore); ok {
		ks.mu.Lock()
		ks.onChange = h.revalidateConns
		ks.mu.Unlock()
	}
	return h, nil
}

// openState opens the configured state backend.
func openState(ctx context.Context, cfg Config) (stateStore, error) {
	if cfg.StateSecret == "" || cfg.Local != nil {
		if cfg.Cluster.Peers != "" && cfg.Local == nil {
			return nil, errors.New("several replicas need a shared state: add --state-secret")
		}
		return openStore(cfg.DataDir)
	}
	api := cfg.k8s
	if api == nil {
		var err error
		if api, err = inClusterAPI(); err != nil {
			return nil, err
		}
	}
	imp := ""
	if cfg.DataDir != "" {
		imp = filepath.Join(cfg.DataDir, "hub.sqlite")
	}
	return openK8sStore(ctx, api, cfg.StateSecret, imp)
}

// revalidateConns drops tunnels whose device another replica deleted or
// whose token it rotated (the state changed under this one).
func (h *Hub) revalidateConns() {
	ds, err := h.st.listDevices(context.Background())
	if err != nil {
		return
	}
	valid := map[string]string{}
	for _, d := range ds {
		valid[d.ID] = d.tokenHash
	}
	var stale []string
	h.mu.Lock()
	for id, dc := range h.conns {
		if hash, ok := valid[id]; !ok || (dc.tokenHash != "" && hash != dc.tokenHash) {
			stale = append(stale, id)
		}
	}
	h.mu.Unlock()
	for _, id := range stale {
		log.Printf("hub: device %s was deleted or its token rotated: dropping its tunnel", id)
		h.dropConn(id)
	}
}

// Close releases the store.
func (h *Hub) Close() error { return h.st.Close() }

// ListenAndServe serves until ctx ends.
func (h *Hub) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", h.cfg.Listen)
	if err != nil {
		return err
	}
	return h.Serve(ctx, ln)
}

// Serve serves on ln until ctx ends.
func (h *Hub) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		h.mu.Lock()
		for _, dc := range h.conns {
			_ = dc.sess.Close()
		}
		h.mu.Unlock()
	}()
	if h.cl != nil {
		go func() {
			if err := h.cl.serve(ctx); err != nil {
				log.Printf("hub: cluster port: %v", err)
			}
		}()
	}
	if h.cfg.Local == nil {
		log.Printf("ccdash hub %s listening on %s (public url %s)", buildinfo.Version, ln.Addr(), h.cfg.PublicURL)
	}
	if n := len(h.readTokens); n > 0 {
		log.Printf("hub: %d read token(s) configured (GET-only machine access)", n)
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler is the hub's full route table.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	api := http.NewServeMux()
	if h.cfg.Local != nil {
		// The local portal has one device, linked in-process (ServeLocal):
		// no tunnel endpoint, no device management, no OIDC.
		mux.HandleFunc("GET /auth/local", h.auth.handleLocalLogin)
		mux.HandleFunc("GET /auth/login", h.auth.handleLocalLoginHelp)
	} else {
		// Device tunnels authenticate with their device token, not a
		// browser session.
		mux.HandleFunc("GET "+tunnel.Path, h.handleAgent)

		mux.HandleFunc("GET /auth/login", h.auth.handleLogin)
		mux.HandleFunc("GET /auth/callback", h.auth.handleCallback)
		mux.HandleFunc("GET /auth/logout", h.auth.handleLogout)

		api.HandleFunc("POST /api/devices", h.handleCreateDevice)
		api.HandleFunc("POST /api/devices/{id}/rotate", h.handleRotateDevice)
		api.HandleFunc("POST /api/devices/{id}/rename", h.handleRenameDevice)
		api.HandleFunc("DELETE /api/devices/{id}", h.handleDeleteDevice)
		api.HandleFunc("POST /api/devices/{id}/peer", h.handlePeerAccess)
	}
	api.HandleFunc("GET /api/me", h.handleMe)
	api.HandleFunc("GET /api/devices", h.handleListDevices)
	api.HandleFunc("GET /api/board", h.handleBoard)
	api.HandleFunc("GET /api/active", h.handleActive)
	api.HandleFunc("GET /api/locate/{session}", h.handleLocate)
	api.HandleFunc("GET /api/quick", h.handleQuickGet)
	api.HandleFunc("PUT /api/quick", h.handleQuickPut)
	api.HandleFunc("GET /api/prefs", h.handlePrefsGet)
	api.HandleFunc("PUT /api/prefs", h.handlePrefsPut)
	api.HandleFunc("GET /api/push/key", h.handlePushKey)
	api.HandleFunc("POST /api/push/subscribe", h.handlePushSubscribe)
	api.HandleFunc("POST /api/push/unsubscribe", h.handlePushUnsubscribe)
	api.HandleFunc("POST /api/push/test", h.handlePushTest)
	api.HandleFunc("GET /api/d/{id}/ws/pty/{key}", h.handlePTYSocket)
	api.HandleFunc("/api/d/{id}/{rest...}", h.handleDeviceProxy)
	// Go 1.25's cross-origin guard rejects non-GET requests a browser
	// marks as cross-site (Sec-Fetch-Site / Origin), so a page elsewhere
	// can't ride the session cookie into a device action. apiAuth lets a
	// read token (Authorization: Bearer) through for a few GET routes and
	// sends everything else to the cookie login.
	mux.Handle("/api/", h.apiAuth(http.NewCrossOriginProtection().Handler(api)))

	static, _ := fs.Sub(webFS, "web")
	files := http.FileServerFS(static)
	// The PWA files are fetched by the browser without the session cookie
	// (manifest requests are credential-less), so they stay outside login.
	// None of them carries data.
	for _, p := range []string{"/manifest.webmanifest", "/sw.js", "/icon.svg", "/icon-192.png", "/icon-512.png"} {
		mux.Handle("GET "+p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			if strings.HasSuffix(r.URL.Path, ".webmanifest") {
				w.Header().Set("Content-Type", "application/manifest+json")
			}
			files.ServeHTTP(w, r)
		}))
	}
	// Vendored libraries (web/vendor, ADR 0005): the version is in the
	// path, so a browser may keep them. Still behind login like the SPA.
	mux.Handle("GET /vendor/", h.requireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	}), true))
	// /s/<session id> is a shareable link to one session's chat (ccdash mcp
	// hands these out); the hub finds the device.
	mux.Handle("GET /s/{session}", h.requireUser(http.HandlerFunc(h.handleSessionLink), true))
	mux.Handle("/", h.requireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		files.ServeHTTP(w, r)
	}), true))
	if h.cfg.Local != nil || h.cfg.Auth.NoAuth || h.cfg.Auth.TailscaleServe {
		return hostGuard(h.cfg.PublicURL, mux)
	}
	return mux
}

// requireUser gates portal routes on a logged-in session. Pages redirect to
// the login flow; API calls get a bare 401 the SPA turns into a redirect.
func (h *Hub) requireUser(next http.Handler, page bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if login, ok := h.auth.user(r); !ok {
			if h.cfg.Local != nil {
				if page {
					h.auth.handleLocalLoginHelp(w, r)
					return
				}
				http.Error(w, "login required", http.StatusUnauthorized)
				return
			}
			if h.cfg.Auth.TailscaleServe {
				h.logRefused(login)
				who := login
				if who == "" {
					who = "(no Tailscale-User-Login — open the hub through `tailscale serve`; tagged devices have no login)"
				}
				http.Error(w, "ccdash hub: "+who+" is not allowed (--allowed-email)", http.StatusForbidden)
				return
			}
			if page {
				http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// assetsHash fingerprints the embedded SPA so open pages can notice a hub
// upgrade (they keep running the old JS otherwise).
func assetsHash() string {
	sum := sha256.New()
	_ = fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := webFS.ReadFile(p)
		if err == nil {
			sum.Write([]byte(p))
			sum.Write(b)
		}
		return err
	})
	return hex.EncodeToString(sum.Sum(nil))[:12]
}

// logRefused logs each refused Tailscale login once.
func (h *Hub) logRefused(login string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.refused == nil {
		h.refused = map[string]bool{}
	}
	if !h.refused[login] {
		h.refused[login] = true
		log.Printf("hub: refused tailscale login %q (not in --allowed-email)", login)
	}
}

// Quick commands: the operator's one-tap prompts ("続けて", "/compact"…),
// kept per login in the hub so every browser / phone shares them.

var defaultQuick = []string{"続けて", "テストを実行して", "変更をコミットして", "/compact"}

func (h *Hub) quickKey(r *http.Request) string {
	email, _ := h.auth.user(r)
	return "quick:" + email
}

func (h *Hub) handleQuickGet(w http.ResponseWriter, r *http.Request) {
	v, err := h.st.secret(r.Context(), h.quickKey(r), func() string {
		b, _ := json.Marshal(defaultQuick)
		return string(b)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(v))
}

func (h *Hub) handleQuickPut(w http.ResponseWriter, r *http.Request) {
	var cmds []string
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&cmds); err != nil {
		http.Error(w, "want a JSON array of strings", http.StatusBadRequest)
		return
	}
	out := []string{}
	for _, c := range cmds {
		if c = strings.TrimSpace(c); c != "" && len(c) <= 2000 && len(out) < 30 {
			out = append(out, c)
		}
	}
	b, _ := json.Marshal(out)
	if err := h.st.putKV(r.Context(), h.quickKey(r), string(b)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, out)
}

// prefs is a portal user's look: theme ("" = follow the system, "light",
// "dark") and accent ("" = the default, else "#rrggbb"). Kept per login
// in hub.sqlite, so the browser and the installed app share it.
type prefs struct {
	Theme  string `json:"theme"`
	Accent string `json:"accent"`
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (h *Hub) prefsKey(r *http.Request) string {
	email, _ := h.auth.user(r)
	return "prefs:" + email
}

func (h *Hub) handlePrefsGet(w http.ResponseWriter, r *http.Request) {
	v, err := h.st.secret(r.Context(), h.prefsKey(r), func() string { return "{}" })
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var p prefs
	_ = json.Unmarshal([]byte(v), &p)
	writeJSON(w, p)
}

func (h *Hub) handlePrefsPut(w http.ResponseWriter, r *http.Request) {
	var p prefs
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&p); err != nil {
		http.Error(w, "want {theme, accent}", http.StatusBadRequest)
		return
	}
	if p.Theme != "" && p.Theme != "light" && p.Theme != "dark" {
		http.Error(w, `theme must be "", "light" or "dark"`, http.StatusBadRequest)
		return
	}
	if p.Accent != "" && !hexColor.MatchString(p.Accent) {
		http.Error(w, "accent must be #rrggbb", http.StatusBadRequest)
		return
	}
	p.Accent = strings.ToLower(p.Accent)
	b, _ := json.Marshal(p)
	if err := h.st.putKV(r.Context(), h.prefsKey(r), string(b)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, p)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Hub) handleMe(w http.ResponseWriter, r *http.Request) {
	email, _ := h.auth.user(r)
	if h.cfg.Local != nil {
		// The SPA polls this every 30 s: keep a portal in daily use logged in.
		h.auth.renewLocal(w, r)
	}
	writeJSON(w, map[string]any{"email": email, "version": buildinfo.Version, "publicUrl": h.cfg.PublicURL,
		"noAuth": h.cfg.Auth.NoAuth || h.cfg.Auth.TailscaleServe || h.cfg.Local != nil,
		"local":  h.cfg.Local != nil, "assets": h.assets})
}

type deviceView struct {
	Device
	Online bool       `json:"online"`
	Since  *time.Time `json:"since,omitempty"`
	Remote string     `json:"remote,omitempty"`
	// Restart is the device's scheduled collector restart, while one is
	// pending or just ended without running (the portal / app warn).
	Restart *model.RestartState `json:"restart,omitempty"`
}

func (h *Hub) handleListDevices(w http.ResponseWriter, r *http.Request) {
	ds, err := h.st.listDevices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]deviceView, 0, len(ds))
	snaps := h.allSnapshots()
	for _, d := range ds {
		v := deviceView{Device: d}
		pc, ok := h.connInfo(d.ID)
		if ok {
			v.Online = true
			v.Since = &pc.Since
			v.Remote = pc.Remote
			if rs := snaps[d.ID].Restart; rs != nil && (rs.Pending || rs.Last != "") {
				v.Restart = rs
			}
		}
		out = append(out, v)
	}
	writeJSON(w, out)
}

func validName(name string) bool {
	return name != "" && len(name) <= 64 && !strings.ContainsAny(name, "\x00\r\n")
}

func (h *Hub) joinCommand() string {
	return "ccdash hub join --url " + h.cfg.PublicURL
}

func (h *Hub) handleCreateDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || !validName(strings.TrimSpace(req.Name)) {
		http.Error(w, "name is required (max 64 chars)", http.StatusBadRequest)
		return
	}
	d, tok, err := h.st.createDevice(r.Context(), strings.TrimSpace(req.Name))
	if errors.Is(err, ErrDuplicateName) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	email, _ := h.auth.user(r)
	log.Printf("hub: %s added device %q (%s)", email, d.Name, d.ID)
	writeJSON(w, map[string]any{"device": d, "token": tok, "join": h.joinCommand()})
}

func (h *Hub) handleRotateDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tok, err := h.st.rotateToken(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.dropConn(id)
	h.dropEverywhere(id)
	email, _ := h.auth.user(r)
	log.Printf("hub: %s rotated the token of device %s", email, id)
	writeJSON(w, map[string]any{"token": tok, "join": h.joinCommand()})
}

func (h *Hub) handleRenameDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || !validName(strings.TrimSpace(req.Name)) {
		http.Error(w, "name is required (max 64 chars)", http.StatusBadRequest)
		return
	}
	err := h.st.renameDevice(r.Context(), r.PathValue("id"), strings.TrimSpace(req.Name))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.NotFound(w, r)
	case errors.Is(err, ErrDuplicateName):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Hub) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.st.deleteDevice(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.dropConn(id)
	h.dropEverywhere(id)
	email, _ := h.auth.user(r)
	log.Printf("hub: %s deleted device %s", email, id)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) dropConn(id string) {
	h.mu.Lock()
	dc := h.conns[id]
	delete(h.conns, id)
	h.mu.Unlock()
	if dc != nil {
		_ = dc.sess.Close()
	}
}

// dropEverywhere makes the other replicas re-read the state and drop
// their tunnel of id (a revoked token must not stay connected there).
func (h *Hub) dropEverywhere(id string) {
	if h.cl != nil {
		h.cl.broadcastDrop(id, time.Time{}, true)
	}
}

// conn is the link to device id: this replica's tunnel, else the replica
// holding it, else nil (offline).
func (h *Hub) conn(id string) *deviceConn {
	if dc := h.localConn(id); dc != nil {
		return dc
	}
	if h.cl != nil {
		return h.cl.conn(id)
	}
	return nil
}

func (h *Hub) localConn(id string) *deviceConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[id]
}

// handleAgent accepts a device tunnel. The newest tunnel for a device wins,
// so a device that reconnects after a network change doesn't wait for the
// old tunnel's keepalive to time out.
func (h *Hub) handleAgent(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		http.Error(w, "device token required", http.StatusUnauthorized)
		return
	}
	d, err := h.st.deviceByToken(r.Context(), tok)
	if err != nil {
		log.Printf("hub: rejected tunnel from %s (unknown token)", clientIP(r))
		http.Error(w, "unknown device token", http.StatusUnauthorized)
		return
	}
	sess, err := tunnel.Accept(w, r)
	if err != nil {
		log.Printf("hub: tunnel accept for %s: %v", d.Name, err)
		return
	}
	h.serveDevice(d, sess, clientIP(r), r.Header.Get(tunnel.HeaderDeviceHost), r.Header.Get(tunnel.HeaderDeviceVersion))
}

// serveDevice makes sess the live link of device d — proxying, the PTY
// socket and push watching all go through it — until sess closes.
func (h *Hub) serveDevice(d Device, sess *yamux.Session, remote, hostname, version string) {
	_ = h.st.touchDevice(context.Background(), d.ID, hostname, version)

	dc := newDeviceConn(sess.Open, time.Now().UTC(), remote)
	dc.sess, dc.tokenHash = sess, d.tokenHash
	tr := dc.transport

	h.mu.Lock()
	old := h.conns[d.ID]
	h.conns[d.ID] = dc
	h.mu.Unlock()
	if old != nil {
		_ = old.sess.Close()
	}
	log.Printf("hub: device %q connected from %s (%s, ccdash %s)", d.Name, dc.remote, hostname, version)
	if h.cl != nil {
		// A tunnel another replica still holds for this device is dead
		// (the device keeps one): close it there so only this one watches.
		go h.cl.broadcastDrop(d.ID, dc.since, false)
	}
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go h.watchDevice(watchCtx, d, dc)

	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for done := false; !done; {
		select {
		case <-sess.CloseChan():
			done = true
		case <-t.C:
			_ = h.st.touchDevice(context.Background(), d.ID, hostname, version)
		}
	}
	h.mu.Lock()
	if h.conns[d.ID] == dc {
		delete(h.conns, d.ID)
	}
	h.mu.Unlock()
	tr.CloseIdleConnections()
	_ = h.st.touchDevice(context.Background(), d.ID, hostname, version)
	log.Printf("hub: device %q disconnected", d.Name)
}

// newDeviceProxy forwards portal requests to a device over tr (the tunnel).
func newDeviceProxy(tr http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "device"
			pr.Out.Host = "device"
			// The device authenticates the tunnel, not the browser: never
			// forward portal credentials.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Ccdash-Token")
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "device unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// deviceProxyPrefixes are the device paths the portal uses. The device's
// own allowlist is the real boundary; this just keeps the hub from being a
// generic pipe.
var deviceProxyPrefixes = []string{"/api/", "/pty", "/approvals/", "/hub/"}

func (h *Hub) handleDeviceProxy(w http.ResponseWriter, r *http.Request) {
	dc := h.conn(r.PathValue("id"))
	if dc == nil {
		http.Error(w, "device is offline", http.StatusServiceUnavailable)
		return
	}
	rest := "/" + r.PathValue("rest")
	if strings.HasSuffix(r.URL.Path, "/") && !strings.HasSuffix(rest, "/") {
		rest += "/"
	}
	allowed := false
	for _, p := range deviceProxyPrefixes {
		if strings.HasPrefix(rest, p) {
			allowed = true
			break
		}
	}
	if !allowed {
		http.NotFound(w, r)
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = rest
	r2.URL.RawPath = ""
	dc.proxy.ServeHTTP(w, r2)
}

// handlePTYSocket bridges a browser WebSocket to the device's raw PTY
// stream (/pty/{key}/stream, the same relay fullscreen attach uses).
// Binary frames carry terminal bytes both ways; text frames from the
// browser are JSON control messages ({"type":"resize","cols":N,"rows":N}).
func (h *Hub) handlePTYSocket(w http.ResponseWriter, r *http.Request) {
	dc := h.conn(r.PathValue("id"))
	if dc == nil {
		http.Error(w, "device is offline", http.StatusServiceUnavailable)
		return
	}
	key := r.PathValue("key")
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))

	// Open the device side first so failures (404 no such PTY, 409 another
	// client attached, 403 attach off) come back as plain HTTP errors the
	// page can show, instead of a WebSocket that closes immediately.
	dev, err := dc.open()
	if err != nil {
		http.Error(w, "device unreachable", http.StatusBadGateway)
		return
	}
	defer dev.Close()
	fmt.Fprintf(dev, "GET /pty/%s/stream HTTP/1.1\r\nHost: device\r\nConnection: Upgrade\r\nUpgrade: pty-raw\r\n\r\n", url.PathEscape(key))
	br := bufio.NewReader(dev)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		http.Error(w, "device stream: "+err.Error(), http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		http.Error(w, strings.TrimSpace(string(msg)), resp.StatusCode)
		return
	}
	if cols > 0 && rows > 0 {
		_ = json.NewEncoder(dev).Encode(map[string]int{"cols": cols, "rows": rows})
	} else {
		_, _ = io.WriteString(dev, "{}\n")
	}

	// Same-origin only. The public host is listed explicitly because a
	// proxy in front (tailscale serve, Caddy) may rewrite Host.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.originPatterns()})
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer ws.CloseNow()

	// device → browser
	go func() {
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = ws.Close(websocket.StatusNormalClosure, "session ended")
				return
			}
		}
	}()
	// keepalive so idle proxies on the browser path don't reap the socket
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pc := context.WithTimeout(ctx, 10*time.Second)
				_ = ws.Ping(pctx)
				pc()
			}
		}
	}()
	// browser → device
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if _, err := dev.Write(data); err != nil {
				return
			}
		case websocket.MessageText:
			var m struct {
				Type string `json:"type"`
				Cols int    `json:"cols"`
				Rows int    `json:"rows"`
			}
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
				go h.resizePTY(dc, key, m.Cols, m.Rows)
			}
		}
	}
}

func (h *Hub) originPatterns() []string {
	if u, err := url.Parse(h.cfg.PublicURL); err == nil && u.Host != "" {
		return []string{u.Host}
	}
	return nil
}

func (h *Hub) resizePTY(dc *deviceConn, key string, cols, rows int) {
	body := fmt.Sprintf(`{"cols":%d,"rows":%d}`, cols, rows)
	req, err := http.NewRequest(http.MethodPost, "http://device/pty/"+url.PathEscape(key)+"/resize", strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: dc.transport, Timeout: 10 * time.Second}).Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
