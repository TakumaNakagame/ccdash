package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/tunnel"
)

// fakeAPI is just enough of the Kubernetes API for one namespace's Secrets:
// get, create (409 when it exists) and update (409 on a stale
// resourceVersion).
type fakeAPI struct {
	mu      sync.Mutex
	secrets map[string]k8sSecret
	rv      int
	writes  int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	const prefix = "/api/v1/namespaces/hub/secrets"
	name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")
	var in k8sSecret
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&in)
	}
	switch r.Method {
	case http.MethodGet:
		s, ok := f.secrets[name]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(s)
	case http.MethodPost:
		if _, ok := f.secrets[in.Metadata.Name]; ok {
			http.Error(w, `{"message":"exists"}`, http.StatusConflict)
			return
		}
		f.store(w, in)
	case http.MethodPut:
		cur, ok := f.secrets[name]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		if in.Metadata.ResourceVersion != cur.Metadata.ResourceVersion {
			http.Error(w, `{"message":"conflict"}`, http.StatusConflict)
			return
		}
		f.store(w, in)
	}
}

func (f *fakeAPI) store(w http.ResponseWriter, s k8sSecret) {
	f.rv++
	f.writes++
	s.Metadata.ResourceVersion = strconv.Itoa(f.rv)
	if f.secrets == nil {
		f.secrets = map[string]k8sSecret{}
	}
	f.secrets[s.Metadata.Name] = s
	_ = json.NewEncoder(w).Encode(s)
}

func newFakeAPI(t *testing.T) (*fakeAPI, *k8sAPI) {
	f := &fakeAPI{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &k8sAPI{base: srv.URL, namespace: "hub", client: srv.Client(),
		token: func() (string, error) { return "test-token", nil }}
}

func TestK8sStoreSharedState(t *testing.T) {
	ctx := context.Background()
	_, api := newFakeAPI(t)
	a, err := openK8sStore(ctx, api, "state", "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := openK8sStore(ctx, api, "state", "")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// Both replicas agree on a generated key even when they race for it.
	var wg sync.WaitGroup
	keys := make([]string, 2)
	for i, s := range []*k8sStore{a, b} {
		wg.Go(func() {
			keys[i], _ = s.secret(ctx, "cookie_key", func() string { return randHex(8) })
		})
	}
	wg.Wait()
	if keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("cookie keys differ: %q", keys)
	}

	// A device joined on a is found on b at once (refresh on a miss).
	d, tok, err := a.createDevice(ctx, "desk")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond) // past the refresh-on-miss rate limit
	got, err := b.deviceByToken(ctx, tok)
	if err != nil || got.ID != d.ID || got.tokenHash == "" {
		t.Fatalf("b.deviceByToken = %+v, %v", got, err)
	}
	// b writes on a stale copy: the conflict is retried, nothing is lost.
	if _, _, err := b.createDevice(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.createDevice(ctx, "phone"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.createDevice(ctx, "desk"); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("duplicate name on b = %v", err)
	}
	if err := a.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	ds, _ := a.listDevices(ctx)
	if len(ds) != 3 || ds[0].Name != "desk" || ds[1].Name != "laptop" || ds[2].Name != "phone" {
		t.Fatalf("devices = %+v", ds)
	}
	if err := a.renameDevice(ctx, d.ID, "laptop"); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("rename onto a taken name = %v", err)
	}

	// Push subscriptions: upsert by endpoint.
	_ = a.addSub(ctx, "me@x", pushSub{Endpoint: "https://push/1", P256dh: "p", Auth: "a"})
	_ = a.addSub(ctx, "me@x", pushSub{Endpoint: "https://push/1", P256dh: "p2", Auth: "a"})
	subs, _ := a.listSubs(ctx)
	if len(subs) != 1 || subs[0].P256dh != "p2" || subs[0].ID != 1 {
		t.Fatalf("subs = %+v", subs)
	}
}

func TestK8sStoreTouchIsThrottled(t *testing.T) {
	ctx := context.Background()
	f, api := newFakeAPI(t)
	s, err := openK8sStore(ctx, api, "state", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, _, _ := s.createDevice(ctx, "desk")
	_ = s.touchDevice(ctx, d.ID, "host", "v1")
	f.mu.Lock()
	before := f.writes
	f.mu.Unlock()
	_ = s.touchDevice(ctx, d.ID, "host", "v1")
	_ = s.touchDevice(ctx, d.ID, "host", "v1")
	f.mu.Lock()
	after := f.writes
	f.mu.Unlock()
	if after != before {
		t.Errorf("unchanged touches wrote the secret %d times", after-before)
	}
	_ = s.touchDevice(ctx, d.ID, "host", "v2")
	if ds, _ := s.listDevices(ctx); ds[0].Version != "v2" {
		t.Errorf("version change not written: %+v", ds[0])
	}
}

func TestK8sStoreImportsSQLite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	old, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, tok, _ := old.createDevice(ctx, "desk")
	_ = old.setPeerAccess(ctx, d.ID, "read")
	key, _ := old.secret(ctx, "cookie_key", func() string { return "k1" })
	_ = old.addSub(ctx, "me@x", pushSub{Endpoint: "https://push/1", P256dh: "p", Auth: "a"})
	_ = old.Close()

	_, api := newFakeAPI(t)
	s, err := openK8sStore(ctx, api, "state", dir+"/hub.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.deviceByToken(ctx, tok)
	if err != nil || got.ID != d.ID || got.PeerAccess != "read" {
		t.Fatalf("imported device = %+v, %v", got, err)
	}
	if k, _ := s.secret(ctx, "cookie_key", func() string { return "new" }); k != key {
		t.Errorf("cookie key = %q, want the imported %q", k, key)
	}
	if subs, _ := s.listSubs(ctx); len(subs) != 1 {
		t.Errorf("subs = %+v", subs)
	}
	// The import runs only into an absent Secret: a later start keeps it.
	if _, _, err := s.createDevice(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
	s2, err := openK8sStore(ctx, api, "state", dir+"/hub.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if ds, _ := s2.listDevices(ctx); len(ds) != 2 {
		t.Errorf("re-import clobbered the secret: %+v", ds)
	}
}

// replica is one hub of a two-replica test cluster.
type replica struct {
	hub     *Hub
	srv     *httptest.Server // the public port
	cluster *httptest.Server // the cluster port
}

func newReplicas(t *testing.T) (*replica, *replica) {
	_, api := newFakeAPI(t)
	mk := func() *replica {
		h, err := New(context.Background(), Config{
			Listen:      "127.0.0.1:0",
			PublicURL:   "https://hub.example",
			Auth:        AuthConfig{NoAuth: true},
			StateSecret: "state",
			Cluster:     ClusterConfig{Peers: "set-below:1"},
			k8s:         api,
		})
		if err != nil {
			t.Fatal(err)
		}
		r := &replica{hub: h, srv: httptest.NewServer(h.Handler()), cluster: httptest.NewServer(h.cl.handler())}
		t.Cleanup(func() { r.srv.Close(); r.cluster.Close(); _ = h.Close() })
		return r
	}
	a, b := mk(), mk()
	peer := func(of *replica) func(context.Context) ([]string, error) {
		addr := strings.TrimPrefix(of.cluster.URL, "http://")
		return func(context.Context) ([]string, error) { return []string{addr}, nil }
	}
	a.hub.cl.resolve, b.hub.cl.resolve = peer(b), peer(a)
	return a, b
}

// connectDevice dials r's tunnel with tok and serves dev on it.
func connectDevice(t *testing.T, r *replica, tok string, dev http.Handler) (stop func()) {
	t.Helper()
	sess, err := tunnel.Dial(context.Background(), r.srv.URL, tok, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = http.Serve(sess, dev) }()
	return func() { _ = sess.Close() }
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestClusterForwardsToTheTunnelsReplica(t *testing.T) {
	a, b := newReplicas(t)
	ctx := context.Background()
	d, tok, err := a.hub.st.createDevice(ctx, "desk")
	if err != nil {
		t.Fatal(err)
	}
	dev := &fakeDevice{sessions: `[{"session_id":"s1","status":"active"}]`, approvals: `[]`, ptys: `[]`}
	stop := connectDevice(t, a, tok, dev)
	waitFor(t, "a holds the tunnel", func() bool { return a.hub.localConn(d.ID) != nil })

	// b has no tunnel: it forwards through a.
	code, body := get(t, b.srv.URL+"/api/d/"+d.ID+"/api/sessions")
	if code != http.StatusOK || !strings.Contains(body, `"s1"`) {
		t.Fatalf("b proxy = %d %s", code, body)
	}
	_ = b.hub.st.(*k8sStore).refresh(ctx) // b's copy of the state lags by up to stateRefresh
	var devs []deviceView
	_, body = get(t, b.srv.URL+"/api/devices")
	_ = json.Unmarshal([]byte(body), &devs)
	if len(devs) != 1 || !devs[0].Online {
		t.Fatalf("b device list = %s", body)
	}
	// a's watcher snapshot shows on b's board.
	waitFor(t, "b sees a's snapshot", func() bool {
		b.hub.cl.refresh(ctx)
		return b.hub.allSnapshots()[d.ID].Device.ID == d.ID
	})
	if b.hub.locateSession("s1") != d.ID {
		t.Error("b can't locate a's session")
	}
	b.hub.mu.Lock()
	watching := len(b.hub.snapshots)
	b.hub.mu.Unlock()
	if watching != 0 {
		t.Error("b watches a device it doesn't hold (pushes would double)")
	}

	// The device reconnects to b: a drops its (dead) tunnel.
	stop()
	stop = connectDevice(t, b, tok, dev)
	defer stop()
	waitFor(t, "b holds the tunnel", func() bool { return b.hub.localConn(d.ID) != nil })
	waitFor(t, "a dropped its tunnel", func() bool { return a.hub.localConn(d.ID) == nil })
	if code, body := get(t, a.srv.URL+"/api/d/"+d.ID+"/api/sessions"); code != http.StatusOK || !strings.Contains(body, `"s1"`) {
		t.Fatalf("a proxy after the move = %d %s", code, body)
	}

	// Deleting the device on a cuts b's tunnel too.
	req, _ := http.NewRequest(http.MethodDelete, a.srv.URL+"/api/devices/"+d.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %v %v", resp, err)
	}
	waitFor(t, "b dropped the deleted device", func() bool { return b.hub.localConn(d.ID) == nil })
}

func TestClusterRejectsUnsignedRequests(t *testing.T) {
	a, _ := newReplicas(t)
	if code, _ := get(t, a.cluster.URL+clusterStatePath); code != http.StatusUnauthorized {
		t.Errorf("unsigned state = %d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, a.cluster.URL+clusterStatePath, nil)
	req.Header.Set(clusterHeader, a.hub.cl.sign("GET", clusterStatePath, time.Now().Add(-2*clusterSkew).Unix()))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("stale signature accepted: %v %v", resp, err)
	}
	other := newCluster(nil, ClusterConfig{}, "another key")
	req.Header.Set(clusterHeader, other.sign("GET", clusterStatePath, time.Now().Unix()))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("foreign key accepted: %v %v", resp, err)
	}
}

func TestClusterNeedsSharedState(t *testing.T) {
	_, err := New(context.Background(), Config{
		Listen: "127.0.0.1:0", DataDir: t.TempDir(), PublicURL: "https://hub.example",
		Auth: AuthConfig{NoAuth: true}, Cluster: ClusterConfig{Peers: "hub-peers"},
	})
	if err == nil {
		t.Fatal("cluster on a private hub.sqlite was accepted")
	}
}
