package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/store"
)

// Multi-host: when this machine is joined to a hub and the operator has
// turned on its peer access there, the tools reach the other joined
// machines' collectors through the hub (store.NewHubPeer → the hub's
// device proxy, authorized by internal/hub/peer.go and then by each
// device's own hub allowlist). This machine's collector is still reached
// directly (s.st). A host is named by its hub device name (or id).
//
// Defaults keep single-host calls working unchanged: read tools without
// host look at every reachable host; tools that act on one host (starting
// a session, projects, the restart) act on this one; a bare "#N" is this
// host's #N, another host's session is "<host>#N" (or its session id).

// Fleet is the hub side of multi-host: the hub origin and this device's
// token (from hub.json).
type Fleet struct {
	hubURL string
	token  string
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	self    *hubDevice
	selfErr error
	selfAt  time.Time
	devs    []hubDevice
	devsAt  time.Time
	stores  map[string]store.Store
}

// NewFleet returns the hub link for the multi-host tools.
func NewFleet(hubURL, deviceToken string) *Fleet {
	return &Fleet{
		hubURL: strings.TrimRight(hubURL, "/"), token: deviceToken,
		client: &http.Client{Timeout: 10 * time.Second}, now: time.Now,
		stores: map[string]store.Store{},
	}
}

type hubDevice struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Hostname   string    `json:"hostname,omitempty"`
	Online     bool      `json:"online"`
	LastSeen   time.Time `json:"last_seen,omitzero"`
	PeerAccess string    `json:"peer_access,omitempty"`
}

func (f *Fleet) get(ctx context.Context, path string, out any) error {
	// Short: a local list_sessions waits on this when the hub is down.
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.hubURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("the hub is unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		msg := strings.TrimSpace(string(b))
		switch resp.StatusCode {
		case http.StatusForbidden:
			return fmt.Errorf("the hub refuses this machine's peer access: %s", msg)
		case http.StatusUnauthorized:
			return fmt.Errorf("the hub refused this machine's device token (%s): the hub predates multi-host (update it), or the token was rotated (re-run `ccdash hub join`)", msg)
		case http.StatusNotFound:
			return fmt.Errorf("the hub is too old for multi-host (update ccdash hub)")
		}
		return fmt.Errorf("hub %s: %s: %s", path, resp.Status, msg)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Self is this machine as the hub knows it. Failures are kept for a short
// while so a hub that refuses doesn't cost a round trip per call.
func (f *Fleet) Self(ctx context.Context) (hubDevice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ttl := 5 * time.Minute
	if f.selfErr != nil {
		ttl = time.Minute
	}
	if (f.self != nil || f.selfErr != nil) && f.now().Sub(f.selfAt) < ttl {
		if f.selfErr != nil {
			return hubDevice{}, f.selfErr
		}
		return *f.self, nil
	}
	var d hubDevice
	err := f.get(ctx, "/api/peer/self", &d)
	f.selfAt = f.now()
	if err != nil {
		f.self, f.selfErr = nil, err
		return hubDevice{}, err
	}
	f.self, f.selfErr = &d, nil
	return d, nil
}

// Devices lists the hub's devices (cached for a few seconds).
func (f *Fleet) Devices(ctx context.Context) ([]hubDevice, error) {
	f.mu.Lock()
	if f.devs != nil && f.now().Sub(f.devsAt) < 5*time.Second {
		defer f.mu.Unlock()
		return f.devs, nil
	}
	f.mu.Unlock()
	var ds []hubDevice
	if err := f.get(ctx, "/api/devices", &ds); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.devs, f.devsAt = ds, f.now()
	f.mu.Unlock()
	return ds, nil
}

func (f *Fleet) storeFor(id string) store.Store {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.stores[id]
	if st == nil {
		st = store.NewHubPeer(f.hubURL+"/api/d/"+url.PathEscape(id), f.token)
		f.stores[id] = st
	}
	return st
}

// attachOn reads a device's attach switch (its /hub/info) for a friendly
// error; the device enforces it on /pty itself either way.
func (f *Fleet) attachOn(ctx context.Context, id string) (bool, error) {
	var info struct {
		AttachEnabled bool `json:"attachEnabled"`
	}
	if err := f.get(ctx, "/api/d/"+url.PathEscape(id)+"/hub/info", &info); err != nil {
		return false, err
	}
	return info.AttachEnabled, nil
}

// WithFleet turns on the multi-host tools.
func (s *Server) WithFleet(f *Fleet) *Server {
	s.fleet = f
	return s
}

// host is one collector the tools talk to.
type host struct {
	name string // hub device name; "" while single-host
	id   string // hub device id; "" for this machine while single-host
	self bool
	st   store.Store
}

// located is a session and the host it lives on.
type located struct {
	x model.Session
	h host
}

// ref is how the tools name a session: "#N" here, "<host>#N" elsewhere.
func (l located) ref() string {
	r := l.x.Ref()
	if r == "" {
		r = l.x.SessionID
	} else if !l.h.self {
		r = l.h.name + r
	}
	return r
}

func refsOfLoc(ls []located) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.ref()
	}
	return out
}

// selfHost is this machine's collector, named when the hub says who it is.
func (s *Server) selfHost(ctx context.Context) host {
	h := host{self: true, st: s.st}
	if s.fleet != nil {
		if d, err := s.fleet.Self(ctx); err == nil {
			h.name, h.id = d.Name, d.ID
		}
	}
	return h
}

// multiHost reports whether other hosts are reachable (and why not).
func (s *Server) multiHost(ctx context.Context) (bool, error) {
	if s.fleet == nil {
		return false, fmt.Errorf("this ccdash isn't joined to a hub (`ccdash hub join`), so only this machine's sessions are reachable")
	}
	if _, err := s.fleet.Self(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func isSelfAlias(name string) bool {
	switch strings.ToLower(name) {
	case "local", "self", "this", "localhost":
		return true
	}
	return false
}

// hostNamed resolves a host argument (device name, case-insensitive, or
// id). Unknown and offline hosts are errors naming the alternatives.
func (s *Server) hostNamed(ctx context.Context, name string) (host, error) {
	name = strings.TrimSpace(name)
	self := s.selfHost(ctx)
	if name == "" || isSelfAlias(name) || self.name != "" && (strings.EqualFold(name, self.name) || name == self.id) {
		return self, nil
	}
	if ok, err := s.multiHost(ctx); !ok {
		return host{}, fmt.Errorf("host %q: %w", name, err)
	}
	ds, err := s.fleet.Devices(ctx)
	if err != nil {
		return host{}, err
	}
	for _, d := range ds {
		if strings.EqualFold(d.Name, name) || d.ID == name {
			if d.ID == self.id {
				return self, nil
			}
			if !d.Online {
				seen := "never connected"
				if !d.LastSeen.IsZero() {
					seen = "last seen " + ago(s.now().Sub(d.LastSeen))
				}
				return host{}, fmt.Errorf("host %q is offline (%s); its collector isn't connected to the hub", d.Name, seen)
			}
			return host{name: d.Name, id: d.ID, st: s.fleet.storeFor(d.ID)}, nil
		}
	}
	names := make([]string, 0, len(ds))
	for _, d := range ds {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return host{}, fmt.Errorf("no host %q (hosts: %s; list_hosts shows them)", name, strings.Join(names, ", "))
}

// hostsFor is the hosts a read tool covers: the named one, or (empty / "*"
// / "all") this one plus every online peer. offline names the rest.
func (s *Server) hostsFor(ctx context.Context, name string) (hs []host, offline []string, err error) {
	name = strings.TrimSpace(name)
	if name != "" && name != "*" && !strings.EqualFold(name, "all") {
		h, err := s.hostNamed(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		return []host{h}, nil, nil
	}
	self := s.selfHost(ctx)
	hs = []host{self}
	if ok, _ := s.multiHost(ctx); !ok {
		return hs, nil, nil
	}
	ds, err := s.fleet.Devices(ctx)
	if err != nil {
		return hs, []string{"(other hosts: " + err.Error() + ")"}, nil
	}
	for _, d := range ds {
		switch {
		case d.ID == self.id:
		case d.Online:
			hs = append(hs, host{name: d.Name, id: d.ID, st: s.fleet.storeFor(d.ID)})
		default:
			offline = append(offline, d.Name)
		}
	}
	return hs, offline, nil
}

// sessionsOn lists sessions on every host in hs at once; a host that fails
// is reported in errs rather than failing the whole call (unless it is the
// only one).
func (s *Server) sessionsOn(ctx context.Context, hs []host, archived bool) ([]located, map[string]string, error) {
	type res struct {
		ss  []model.Session
		err error
	}
	out := make([]res, len(hs))
	var wg sync.WaitGroup
	for i, h := range hs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ss, err := h.st.ListSessions(ctx, false)
			if err == nil && archived {
				var more []model.Session
				if more, err = h.st.ListSessions(ctx, true); err == nil {
					ss = append(ss, more...)
				}
			}
			out[i] = res{ss, err}
		}()
	}
	wg.Wait()
	var ls []located
	errs := map[string]string{}
	for i, r := range out {
		if r.err != nil {
			if len(hs) == 1 {
				return nil, nil, r.err
			}
			errs[hostLabel(hs[i])] = r.err.Error()
			continue
		}
		for _, x := range r.ss {
			ls = append(ls, located{x, hs[i]})
		}
	}
	return ls, errs, nil
}

func hostLabel(h host) string {
	if h.name == "" {
		return "this machine"
	}
	return h.name
}

// splitRef takes a host prefix off "<host>#N".
func splitRef(ref string) (hostName, rest string) {
	ref = strings.TrimSpace(ref)
	if i := strings.LastIndex(ref, "#"); i > 0 {
		if _, err := strconv.ParseInt(ref[i+1:], 10, 64); err == nil {
			return strings.TrimSpace(ref[:i]), ref[i:]
		}
	}
	return "", ref
}

func findIn(ss []model.Session, ref string) (model.Session, bool) {
	num, numErr := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64)
	for _, x := range ss {
		if x.SessionID == ref || numErr == nil && x.Num == num {
			return x, true
		}
	}
	return model.Session{}, false
}

// locate resolves "#N", "N", "<host>#N" or a session id. hostArg (the
// tool's host argument) picks the host for a bare number; a session id
// with no host is looked for here first, then on the other hosts.
func (s *Server) locate(ctx context.Context, ref, hostArg string) (located, error) {
	prefix, rest := splitRef(ref)
	if prefix != "" && hostArg != "" && !strings.EqualFold(prefix, hostArg) {
		return located{}, fmt.Errorf("%q names host %q but host is %q", ref, prefix, hostArg)
	}
	if prefix == "" {
		prefix = hostArg
	}
	var hs []host
	if prefix != "" {
		h, err := s.hostNamed(ctx, prefix)
		if err != nil {
			return located{}, err
		}
		hs = []host{h}
	} else {
		hs = []host{s.selfHost(ctx)}
		if _, err := strconv.ParseInt(strings.TrimPrefix(rest, "#"), 10, 64); err != nil {
			if all, _, err := s.hostsFor(ctx, ""); err == nil {
				hs = all
			}
		}
	}
	for _, h := range hs {
		for _, archived := range []bool{false, true} {
			ss, err := h.st.ListSessions(ctx, archived)
			if err != nil {
				if len(hs) == 1 {
					return located{}, err
				}
				break
			}
			if x, ok := findIn(ss, rest); ok {
				return located{x, h}, nil
			}
		}
	}
	where := ""
	if prefix != "" {
		where = " on host " + hs[0].name
	}
	return located{}, fmt.Errorf("no session %q%s (use #N, <host>#N or a session id; list_sessions shows them)", ref, where)
}

// locateAll resolves refs, failing on the first unknown one so nothing is
// half-applied.
func (s *Server) locateAll(ctx context.Context, refs []string, hostArg string) ([]located, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("sessions is required")
	}
	out := make([]located, 0, len(refs))
	for _, r := range refs {
		l, err := s.locate(ctx, r, hostArg)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// listHosts is the list_hosts tool.
func (s *Server) listHosts(ctx context.Context) (any, error) {
	self := s.selfHost(ctx)
	if ok, err := s.multiHost(ctx); !ok {
		return map[string]any{
			"hosts": []map[string]any{{"name": hostLabel(self), "self": true, "online": true}},
			"note":  "only this machine is reachable: " + err.Error(),
		}, nil
	}
	ds, err := s.fleet.Devices(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(ds))
	for _, d := range ds {
		r := map[string]any{"name": d.Name, "id": d.ID, "online": d.Online}
		if d.Hostname != "" {
			r["hostname"] = d.Hostname
		}
		if d.ID == self.id {
			r["self"] = true
		}
		if !d.Online && !d.LastSeen.IsZero() {
			r["last_seen_ago"] = ago(s.now().Sub(d.LastSeen))
		}
		rows = append(rows, r)
	}
	return map[string]any{"hosts": rows, "note": "pass a name as host; self is the machine this MCP server runs on (the default for start_session)"}, nil
}
