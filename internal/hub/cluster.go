package hub

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Several hub replicas (docs/decisions/0008-hub-ha.md). Each device keeps
// ONE tunnel, to whichever replica the load balancer gave it; a replica
// asked for a device it doesn't hold forwards through the replica that
// does. Replicas find each other by a headless Service's DNS name, talk on
// a separate cluster port (never behind the ingress), and sign every
// request with the cluster key from the shared state store.
//
// Each replica polls the others' /cluster/state every clusterRefresh: which
// devices they hold and the watchers' snapshots, so the board, the device
// list and the push badge cover every device. Only the replica holding a
// device's tunnel watches it, so a change is pushed once.

// ClusterConfig turns on replica forwarding. Empty Peers = a lone hub.
type ClusterConfig struct {
	// Peers is a DNS name whose A/AAAA records are the replicas (a
	// headless Service), or a comma-separated host:port list.
	Peers string
	// Listen is the cluster port, e.g. ":8081".
	Listen string
	// Advertise is this replica's own host:port as the others reach it
	// (POD_IP:8081), so it skips itself in the peer list.
	Advertise string
}

const (
	clusterRefresh = 2 * time.Second
	// clusterStale drops a peer that hasn't answered for this long.
	clusterStale = 3 * clusterRefresh
	// clusterSkew bounds the age of a signed request.
	clusterSkew       = time.Minute
	clusterHeader     = "X-Ccdash-Cluster"
	clusterUpgrade    = "ccdash-device-stream"
	clusterKeyName    = "cluster_key"
	clusterStatePath  = "/cluster/state"
	clusterStreamPath = "/cluster/stream/"
	clusterDropPath   = "/cluster/drop/"
)

type cluster struct {
	h      *Hub
	cfg    ClusterConfig
	key    []byte
	client *http.Client

	mu        sync.Mutex
	peers     map[string]*peerView                    // peer addr → its last answer
	remote    map[string]*deviceConn                  // peer addr + "\t" + device id → forwarding conn
	lastForce time.Time                               // last on-demand refresh (conn miss)
	resolve   func(context.Context) ([]string, error) // tests replace it
}

type peerView struct {
	at        time.Time
	conns     map[string]peerConn
	snapshots []deviceSnapshot
}

// peerConn is one tunnel a peer holds, as /cluster/state reports it.
type peerConn struct {
	ID     string    `json:"id"`
	Since  time.Time `json:"since"`
	Remote string    `json:"remote,omitempty"`
}

type clusterState struct {
	Conns     []peerConn       `json:"conns"`
	Snapshots []deviceSnapshot `json:"snapshots"`
}

func newCluster(h *Hub, cfg ClusterConfig, key string) *cluster {
	c := &cluster{h: h, cfg: cfg, key: []byte(key),
		client: &http.Client{Timeout: 3 * time.Second},
		peers:  map[string]*peerView{}, remote: map[string]*deviceConn{}}
	c.resolve = c.resolvePeers
	return c
}

func (c *cluster) port() string {
	_, p, err := net.SplitHostPort(c.cfg.Listen)
	if err != nil || p == "" {
		return "8081"
	}
	return p
}

// resolvePeers lists the other replicas' cluster addresses.
func (c *cluster) resolvePeers(ctx context.Context) ([]string, error) {
	var addrs []string
	if strings.Contains(c.cfg.Peers, ",") || strings.Contains(c.cfg.Peers, ":") {
		for a := range strings.SplitSeq(c.cfg.Peers, ",") {
			if a = strings.TrimSpace(a); a != "" {
				addrs = append(addrs, a)
			}
		}
	} else {
		ips, err := net.DefaultResolver.LookupHost(ctx, c.cfg.Peers)
		if err != nil {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				return nil, nil // no ready replica published yet
			}
			return nil, err
		}
		for _, ip := range ips {
			addrs = append(addrs, net.JoinHostPort(ip, c.port()))
		}
	}
	return slices.DeleteFunc(addrs, func(a string) bool { return a == c.cfg.Advertise }), nil
}

// ---- signing ----

func (c *cluster) sign(method, uri string, ts int64) string {
	m := hmac.New(sha256.New, c.key)
	fmt.Fprintf(m, "%d\n%s\n%s", ts, method, uri)
	return strconv.FormatInt(ts, 10) + "." + hex.EncodeToString(m.Sum(nil))
}

func (c *cluster) verify(r *http.Request) bool {
	tsStr, _, ok := strings.Cut(r.Header.Get(clusterHeader), ".")
	if !ok {
		return false
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)).Abs() > clusterSkew {
		return false
	}
	want := c.sign(r.Method, r.URL.RequestURI(), ts)
	return hmac.Equal([]byte(r.Header.Get(clusterHeader)), []byte(want))
}

func (c *cluster) newRequest(ctx context.Context, method, addr, uri string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+uri, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(clusterHeader, c.sign(method, uri, time.Now().Unix()))
	return req, nil
}

// ---- the cluster port ----

func (c *cluster) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+clusterStatePath, c.handleState)
	mux.HandleFunc("GET "+clusterStreamPath+"{id}", c.handleStream)
	mux.HandleFunc("POST "+clusterDropPath+"{id}", c.handleDrop)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.verify(r) {
			http.Error(w, "bad cluster signature", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (c *cluster) serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", c.cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: c.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go c.loop(ctx)
	log.Printf("hub: cluster port %s (peers %s, me %s)", ln.Addr(), c.cfg.Peers, c.cfg.Advertise)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (c *cluster) handleState(w http.ResponseWriter, r *http.Request) {
	h := c.h
	st := clusterState{Conns: []peerConn{}, Snapshots: []deviceSnapshot{}}
	h.mu.Lock()
	for id, dc := range h.conns {
		st.Conns = append(st.Conns, peerConn{ID: id, Since: dc.since, Remote: dc.remote})
	}
	for id, snap := range h.snapshots {
		if h.conns[id] != nil {
			st.Snapshots = append(st.Snapshots, snap)
		}
	}
	h.mu.Unlock()
	writeJSON(w, st)
}

// handleStream turns the request's connection into a raw stream to the
// device's collector: one yamux stream of the tunnel this replica holds.
func (c *cluster) handleStream(w http.ResponseWriter, r *http.Request) {
	dc := c.h.localConn(r.PathValue("id"))
	if dc == nil {
		http.Error(w, "device not on this replica", http.StatusNotFound)
		return
	}
	dev, err := dc.sess.Open()
	if err != nil {
		http.Error(w, "device unreachable", http.StatusBadGateway)
		return
	}
	defer dev.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + clusterUpgrade + "\r\n\r\n")
	if err := brw.Flush(); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(dev, brw.Reader); _ = dev.Close(); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, dev); _ = conn.Close(); done <- struct{}{} }()
	<-done
}

// handleDrop closes this replica's tunnel of a device: it reconnected to
// another replica (since = that tunnel's start; an older one here is
// dead), or its token was rotated / the device deleted (reload=1: re-read
// the state first, so the old token can't connect again here).
func (c *cluster) handleDrop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.URL.Query().Get("reload") == "1" {
		if ks, ok := c.h.st.(*k8sStore); ok {
			_ = ks.refresh(r.Context())
		}
	}
	var since time.Time
	if ns, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil && ns > 0 {
		since = time.Unix(0, ns)
	}
	h := c.h
	h.mu.Lock()
	dc := h.conns[id]
	drop := dc != nil && (since.IsZero() || dc.since.Before(since))
	h.mu.Unlock()
	if drop {
		log.Printf("hub: dropping device %s's tunnel here (moved to another replica or revoked)", id)
		h.dropConn(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// broadcastDrop tells every peer to drop its tunnel of id (see handleDrop).
func (c *cluster) broadcastDrop(id string, since time.Time, reload bool) {
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", strconv.FormatInt(since.UnixNano(), 10))
	}
	if reload {
		q.Set("reload", "1")
	}
	uri := clusterDropPath + url.PathEscape(id) + "?" + q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addrs, err := c.resolve(ctx)
	if err != nil {
		log.Printf("hub: cluster peers: %v", err)
		return
	}
	var wg sync.WaitGroup
	for _, a := range addrs {
		wg.Go(func() {
			req, err := c.newRequest(ctx, http.MethodPost, a, uri)
			if err != nil {
				return
			}
			resp, err := c.client.Do(req)
			if err != nil {
				log.Printf("hub: cluster drop %s on %s: %v", id, a, err)
				return
			}
			resp.Body.Close()
		})
	}
	wg.Wait()
}

// ---- polling the peers ----

func (c *cluster) loop(ctx context.Context) {
	t := time.NewTicker(clusterRefresh)
	defer t.Stop()
	for {
		c.refresh(ctx)
		select {
		case <-ctx.Done():
			c.mu.Lock()
			for _, dc := range c.remote {
				dc.transport.CloseIdleConnections()
			}
			c.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

func (c *cluster) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addrs, err := c.resolve(ctx)
	if err != nil {
		log.Printf("hub: cluster peers: %v", err)
		return
	}
	type answer struct {
		addr string
		st   clusterState
		err  error
	}
	ch := make(chan answer, len(addrs))
	for _, a := range addrs {
		go func() {
			var st clusterState
			err := c.getJSON(ctx, a, clusterStatePath, &st)
			ch <- answer{a, st, err}
		}()
	}
	now := time.Now()
	got := map[string]clusterState{}
	for range addrs {
		ans := <-ch
		if ans.err == nil {
			got[ans.addr] = ans.st
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for a, st := range got {
		pv := &peerView{at: now, conns: map[string]peerConn{}, snapshots: st.Snapshots}
		for _, pc := range st.Conns {
			pv.conns[pc.ID] = pc
		}
		c.peers[a] = pv
	}
	for a, pv := range c.peers {
		if !slices.Contains(addrs, a) || now.Sub(pv.at) > clusterStale {
			delete(c.peers, a)
		}
	}
	for k, dc := range c.remote {
		a, id, _ := strings.Cut(k, "\t")
		if pv := c.peers[a]; pv == nil || pv.conns[id].ID == "" {
			dc.transport.CloseIdleConnections()
			delete(c.remote, k)
		}
	}
}

func (c *cluster) getJSON(ctx context.Context, addr, uri string, out any) error {
	req, err := c.newRequest(ctx, http.MethodGet, addr, uri)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s%s: %s", addr, uri, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// owner names the peer holding id's tunnel (the newest, if two claim it).
func (c *cluster) ownerLocked(id string) (string, peerConn) {
	var addr string
	var best peerConn
	for a, pv := range c.peers {
		if pc, ok := pv.conns[id]; ok && (addr == "" || pc.Since.After(best.Since)) {
			addr, best = a, pc
		}
	}
	return addr, best
}

// conn returns a deviceConn that forwards to the peer holding id, or nil.
// A miss refreshes the peers once (rate-limited): the device may have just
// reconnected to another replica.
func (c *cluster) conn(id string) *deviceConn {
	c.mu.Lock()
	addr, pc := c.ownerLocked(id)
	if addr == "" && time.Since(c.lastForce) > time.Second {
		c.lastForce = time.Now()
		c.mu.Unlock()
		c.refresh(context.Background())
		c.mu.Lock()
		addr, pc = c.ownerLocked(id)
	}
	defer c.mu.Unlock()
	if addr == "" {
		return nil
	}
	k := addr + "\t" + id
	if dc := c.remote[k]; dc != nil {
		return dc
	}
	dc := newDeviceConn(func() (net.Conn, error) { return c.dialStream(addr, id) }, pc.Since, pc.Remote)
	dc.peer = addr
	c.remote[k] = dc
	return dc
}

// info reports a peer-held tunnel for the device list.
func (c *cluster) info(id string) (peerConn, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	addr, pc := c.ownerLocked(id)
	return pc, addr != ""
}

func (c *cluster) snapshots() []deviceSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []deviceSnapshot
	for _, pv := range c.peers {
		out = append(out, pv.snapshots...)
	}
	return out
}

func (c *cluster) onlineIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, pv := range c.peers {
		for id := range pv.conns {
			out = append(out, id)
		}
	}
	return out
}

// dialStream opens a raw stream to device id through the peer at addr.
func (c *cluster) dialStream(addr, id string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	uri := clusterStreamPath + url.PathEscape(id)
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: %s\r\n%s: %s\r\n\r\n",
		uri, addr, clusterUpgrade, clusterHeader, c.sign(http.MethodGet, uri, time.Now().Unix()))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("replica %s: %s %s", addr, resp.Status, strings.TrimSpace(string(msg)))
	}
	_ = conn.SetReadDeadline(time.Time{})
	return &bufConn{Conn: conn, r: br}, nil
}

// bufConn reads what the bufio.Reader already buffered past the 101.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }
