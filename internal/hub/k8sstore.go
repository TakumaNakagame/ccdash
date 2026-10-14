package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"slices"
	"strings"
	"sync"
	"time"
)

// k8sStore keeps the hub's state as one JSON document in a Kubernetes
// Secret (--state-secret), so hub replicas share it without a volume: a
// ReadWriteOnce disk had to move with the Pod, which took about a minute
// on every node drain. The state is small and rarely written (joins, token
// rotations, push subscriptions, prefs, a last-seen stamp), so every write
// is a read-modify-write of the whole document guarded by the Secret's
// resourceVersion, and every read is served from a copy refreshed every
// stateRefresh (and at once when a device token is not found, so a device
// joined through another replica connects without waiting).
type k8sStore struct {
	api  *k8sAPI
	name string

	mu       sync.Mutex
	doc      stateDoc
	rv       string // resourceVersion of doc; "" = the Secret does not exist yet
	loadedAt time.Time

	// onChange runs after a refresh brought in another replica's write.
	onChange func()

	stop chan struct{}
	done chan struct{}
}

const (
	stateKey     = "state.json"
	stateRefresh = 2 * time.Second
	// touchEvery limits last-seen writes: each one rewrites the Secret.
	touchEvery = 5 * time.Minute
)

type stateDoc struct {
	Devices   []docDevice       `json:"devices"`
	KV        map[string]string `json:"kv"`
	Subs      []docSub          `json:"push_subs"`
	NextSubID int64             `json:"next_sub_id"`
}

type docDevice struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	TokenHash  string    `json:"token_hash"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeen   time.Time `json:"last_seen,omitzero"`
	Hostname   string    `json:"hostname,omitempty"`
	Version    string    `json:"version,omitempty"`
	PeerAccess string    `json:"peer_access,omitempty"`
	Letter     string    `json:"letter,omitempty"`
}

type docSub struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	Endpoint  string    `json:"endpoint"`
	P256dh    string    `json:"p256dh"`
	Auth      string    `json:"auth"`
	CreatedAt time.Time `json:"created_at"`
}

func (d docDevice) device() Device {
	return Device{ID: d.ID, Name: d.Name, CreatedAt: d.CreatedAt, LastSeen: d.LastSeen,
		Hostname: d.Hostname, Version: d.Version, PeerAccess: d.PeerAccess, Letter: d.Letter, tokenHash: d.TokenHash}
}

func (s docSub) sub() pushSub {
	return pushSub{ID: s.ID, Email: s.Email, Endpoint: s.Endpoint, P256dh: s.P256dh, Auth: s.Auth}
}

func (d *stateDoc) clone() stateDoc {
	c := stateDoc{Devices: slices.Clone(d.Devices), Subs: slices.Clone(d.Subs), NextSubID: d.NextSubID, KV: make(map[string]string, len(d.KV))}
	for k, v := range d.KV {
		c.KV[k] = v
	}
	return c
}

func (d *stateDoc) device(id string) *docDevice {
	for i := range d.Devices {
		if d.Devices[i].ID == id {
			return &d.Devices[i]
		}
	}
	return nil
}

func (d *stateDoc) nameTaken(name, except string) bool {
	for _, x := range d.Devices {
		if x.Name == name && x.ID != except {
			return true
		}
	}
	return false
}

// openK8sStore loads (or, with importFrom naming an existing hub.sqlite and
// no Secret yet, seeds) the state Secret and starts the refresh loop.
func openK8sStore(ctx context.Context, api *k8sAPI, name, importFrom string) (*k8sStore, error) {
	s := &k8sStore{api: api, name: name, stop: make(chan struct{}), done: make(chan struct{})}
	if err := s.refresh(ctx); err != nil {
		return nil, fmt.Errorf("state secret %s/%s: %w", api.namespace, name, err)
	}
	if s.rv == "" && importFrom != "" {
		if _, err := os.Stat(importFrom); err == nil {
			doc, err := readSQLiteState(ctx, importFrom)
			if err != nil {
				return nil, fmt.Errorf("import %s: %w", importFrom, err)
			}
			err = s.mutate(ctx, func(d *stateDoc) error {
				if len(d.Devices) > 0 || len(d.KV) > 0 {
					return errNoChange // another replica imported first
				}
				*d = doc
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("import %s: %w", importFrom, err)
			}
			log.Printf("hub: imported %s into secret %s (%d devices, %d push subscriptions)", importFrom, name, len(doc.Devices), len(doc.Subs))
		}
	}
	// Devices from before letters get one, in registration order.
	err := s.mutate(ctx, func(d *stateDoc) error {
		if !d.backfillLetters() {
			return errNoChange
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("state secret %s/%s: letters: %w", api.namespace, name, err)
	}
	go s.loop()
	return s, nil
}

func (d *stateDoc) usedLetters() map[string]bool {
	used := map[string]bool{}
	for _, x := range d.Devices {
		if x.Letter != "" {
			used[x.Letter] = true
		}
	}
	return used
}

// backfillLetters gives every device without a letter one (registration
// order); false when there was nothing to do.
func (d *stateDoc) backfillLetters() bool {
	idx := []int{}
	for i, x := range d.Devices {
		if x.Letter == "" && x.ID != LocalDeviceID {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return false
	}
	sort.SliceStable(idx, func(a, b int) bool {
		da, db := d.Devices[idx[a]], d.Devices[idx[b]]
		if !da.CreatedAt.Equal(db.CreatedAt) {
			return da.CreatedAt.Before(db.CreatedAt)
		}
		return da.ID < db.ID
	})
	used := d.usedLetters()
	for _, i := range idx {
		l := nextLetter(used)
		d.Devices[i].Letter = l
		used[l] = true
	}
	return true
}

func (s *k8sStore) loop() {
	defer close(s.done)
	t := time.NewTicker(stateRefresh)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.refresh(ctx); err != nil {
				log.Printf("hub: refresh state: %v", err)
			}
			cancel()
		}
	}
}

func (s *k8sStore) Close() error {
	select {
	case <-s.stop:
	default:
		close(s.stop)
		<-s.done
	}
	return nil
}

// refresh re-reads the Secret; onChange runs when it changed under us.
func (s *k8sStore) refresh(ctx context.Context) error {
	doc, rv, err := s.api.getState(ctx, s.name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	changed := rv != s.rv
	if changed {
		s.doc, s.rv = doc, rv
	}
	s.loadedAt = time.Now()
	cb := s.onChange
	s.mu.Unlock()
	if changed && cb != nil {
		cb()
	}
	return nil
}

var errNoChange = errors.New("no change")

// mutate applies fn to the latest document and writes it back, retrying
// on a conflict with another replica's write. fn returning errNoChange
// skips the write; any other error is returned as is.
func (s *k8sStore) mutate(ctx context.Context, fn func(*stateDoc) error) error {
	for attempt := 0; ; attempt++ {
		s.mu.Lock()
		doc, rv := s.doc.clone(), s.rv
		s.mu.Unlock()
		if err := fn(&doc); err != nil {
			if errors.Is(err, errNoChange) {
				return nil
			}
			return err
		}
		newRV, err := s.api.putState(ctx, s.name, doc, rv)
		if err == nil {
			s.mu.Lock()
			s.doc, s.rv, s.loadedAt = doc, newRV, time.Now()
			s.mu.Unlock()
			return nil
		}
		if !errors.Is(err, errConflict) || attempt >= 8 {
			return err
		}
		if err := s.refresh(ctx); err != nil {
			return err
		}
	}
}

func (s *k8sStore) read(fn func(*stateDoc)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.doc)
}

func (s *k8sStore) secret(ctx context.Context, k string, gen func() string) (string, error) {
	var v string
	var ok bool
	s.read(func(d *stateDoc) { v, ok = d.KV[k] })
	if ok {
		return v, nil
	}
	err := s.mutate(ctx, func(d *stateDoc) error {
		if cur, ok := d.KV[k]; ok {
			v = cur
			return errNoChange
		}
		v = gen()
		d.KV[k] = v
		return nil
	})
	return v, err
}

func (s *k8sStore) putKV(ctx context.Context, k, v string) error {
	return s.mutate(ctx, func(d *stateDoc) error {
		if cur, ok := d.KV[k]; ok && cur == v {
			return errNoChange
		}
		d.KV[k] = v
		return nil
	})
}

func (s *k8sStore) createDevice(ctx context.Context, name string) (Device, string, error) {
	nd := docDevice{ID: randHex(8), Name: name, CreatedAt: time.Now().UTC()}
	tok := "ccdh_" + randHex(32)
	nd.TokenHash = hashToken(tok)
	err := s.mutate(ctx, func(d *stateDoc) error {
		if d.nameTaken(name, "") {
			return ErrDuplicateName
		}
		nd.Letter = nextLetter(d.usedLetters())
		d.Devices = append(d.Devices, nd)
		return nil
	})
	if err != nil {
		return Device{}, "", err
	}
	return nd.device(), tok, nil
}

func (s *k8sStore) rotateToken(ctx context.Context, id string) (string, error) {
	tok := "ccdh_" + randHex(32)
	err := s.mutate(ctx, func(d *stateDoc) error {
		x := d.device(id)
		if x == nil {
			return sql.ErrNoRows
		}
		x.TokenHash = hashToken(tok)
		return nil
	})
	if err != nil {
		return "", err
	}
	return tok, nil
}

func (s *k8sStore) deleteDevice(ctx context.Context, id string) error {
	return s.mutate(ctx, func(d *stateDoc) error {
		i := slices.IndexFunc(d.Devices, func(x docDevice) bool { return x.ID == id })
		if i < 0 {
			return sql.ErrNoRows
		}
		d.Devices = slices.Delete(d.Devices, i, i+1)
		return nil
	})
}

func (s *k8sStore) renameDevice(ctx context.Context, id, name string) error {
	return s.mutate(ctx, func(d *stateDoc) error {
		x := d.device(id)
		if x == nil {
			return sql.ErrNoRows
		}
		if d.nameTaken(name, id) {
			return ErrDuplicateName
		}
		x.Name = name
		return nil
	})
}

func (s *k8sStore) listDevices(context.Context) ([]Device, error) {
	out := []Device{}
	s.read(func(d *stateDoc) {
		for _, x := range d.Devices {
			out = append(out, x.device())
		}
	})
	slices.SortFunc(out, func(a, b Device) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *k8sStore) deviceByToken(ctx context.Context, tok string) (Device, error) {
	h := hashToken(tok)
	find := func() (Device, bool) {
		var out Device
		var ok bool
		s.read(func(d *stateDoc) {
			for _, x := range d.Devices {
				if x.TokenHash == h {
					out, ok = x.device(), true
					return
				}
			}
		})
		return out, ok
	}
	if d, ok := find(); ok {
		return d, nil
	}
	// Maybe joined (or rotated) through another replica a moment ago.
	s.mu.Lock()
	stale := time.Since(s.loadedAt) > 500*time.Millisecond
	s.mu.Unlock()
	if stale && s.refresh(ctx) == nil {
		if d, ok := find(); ok {
			return d, nil
		}
	}
	return Device{}, sql.ErrNoRows
}

func (s *k8sStore) touchDevice(ctx context.Context, id, hostname, version string) error {
	now := time.Now().UTC()
	return s.mutate(ctx, func(d *stateDoc) error {
		x := d.device(id)
		if x == nil {
			return errNoChange
		}
		if x.Hostname == hostname && x.Version == version && now.Sub(x.LastSeen) < touchEvery {
			return errNoChange
		}
		x.LastSeen, x.Hostname, x.Version = now, hostname, version
		return nil
	})
}

func (s *k8sStore) setPeerAccess(ctx context.Context, id, access string) error {
	return s.mutate(ctx, func(d *stateDoc) error {
		x := d.device(id)
		if x == nil {
			return sql.ErrNoRows
		}
		x.PeerAccess = access
		return nil
	})
}

func (s *k8sStore) setLetter(ctx context.Context, id, letter string) error {
	return s.mutate(ctx, func(d *stateDoc) error {
		x := d.device(id)
		if x == nil {
			return sql.ErrNoRows
		}
		for _, o := range d.Devices {
			if o.ID != id && o.Letter == letter {
				return ErrDuplicateLetter
			}
		}
		x.Letter = letter
		return nil
	})
}

func (s *k8sStore) ensureLocalDevice(ctx context.Context, name string) (Device, error) {
	if !validName(name) {
		name = "this machine"
	}
	var out Device
	err := s.mutate(ctx, func(d *stateDoc) error {
		if x := d.device(LocalDeviceID); x != nil {
			x.Name = name
			out = x.device()
			return nil
		}
		nd := docDevice{ID: LocalDeviceID, Name: name, TokenHash: hashToken(randHex(32)), CreatedAt: time.Now().UTC()}
		d.Devices = append(d.Devices, nd)
		out = nd.device()
		return nil
	})
	return out, err
}

func (s *k8sStore) addSub(ctx context.Context, email string, sub pushSub) error {
	return s.mutate(ctx, func(d *stateDoc) error {
		for i := range d.Subs {
			if d.Subs[i].Endpoint == sub.Endpoint {
				d.Subs[i].Email, d.Subs[i].P256dh, d.Subs[i].Auth = email, sub.P256dh, sub.Auth
				return nil
			}
		}
		d.NextSubID++
		d.Subs = append(d.Subs, docSub{ID: d.NextSubID, Email: email, Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth, CreatedAt: time.Now().UTC()})
		return nil
	})
}

func (s *k8sStore) removeSub(ctx context.Context, endpoint string) error {
	return s.mutate(ctx, func(d *stateDoc) error {
		i := slices.IndexFunc(d.Subs, func(x docSub) bool { return x.Endpoint == endpoint })
		if i < 0 {
			return errNoChange
		}
		d.Subs = slices.Delete(d.Subs, i, i+1)
		return nil
	})
}

func (s *k8sStore) subByEndpoint(_ context.Context, endpoint string) (pushSub, error) {
	var out pushSub
	found := false
	s.read(func(d *stateDoc) {
		for _, x := range d.Subs {
			if x.Endpoint == endpoint {
				out, found = x.sub(), true
				return
			}
		}
	})
	if !found {
		return pushSub{}, sql.ErrNoRows
	}
	return out, nil
}

func (s *k8sStore) listSubs(context.Context) ([]pushSub, error) {
	var out []pushSub
	s.read(func(d *stateDoc) {
		for _, x := range d.Subs {
			out = append(out, x.sub())
		}
	})
	return out, nil
}

var _ stateStore = (*k8sStore)(nil)

// readSQLiteState turns an existing hub.sqlite into a state document (the
// one-time move to --state-secret).
func readSQLiteState(ctx context.Context, path string) (stateDoc, error) {
	st, err := openStoreFile(path)
	if err != nil {
		return stateDoc{}, err
	}
	defer st.Close()
	doc := stateDoc{KV: map[string]string{}}
	ds, err := st.listDevices(ctx)
	if err != nil {
		return stateDoc{}, err
	}
	for _, d := range ds {
		doc.Devices = append(doc.Devices, docDevice{ID: d.ID, Name: d.Name, TokenHash: d.tokenHash, CreatedAt: d.CreatedAt,
			LastSeen: d.LastSeen, Hostname: d.Hostname, Version: d.Version, PeerAccess: d.PeerAccess, Letter: d.Letter})
	}
	rows, err := st.db.QueryContext(ctx, `SELECT k, v FROM kv`)
	if err != nil {
		return stateDoc{}, err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return stateDoc{}, err
		}
		doc.KV[k] = v
	}
	rows.Close()
	subs, err := st.listSubs(ctx)
	if err != nil {
		return stateDoc{}, err
	}
	for _, p := range subs {
		doc.Subs = append(doc.Subs, docSub{ID: p.ID, Email: p.Email, Endpoint: p.Endpoint, P256dh: p.P256dh, Auth: p.Auth, CreatedAt: time.Now().UTC()})
		doc.NextSubID = max(doc.NextSubID, p.ID)
	}
	return doc, nil
}

// ---- a minimal Kubernetes API client: get / create / update one Secret ----

type k8sAPI struct {
	base      string // https://kubernetes.default.svc:443
	namespace string
	client    *http.Client
	token     func() (string, error)
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// inClusterAPI talks to the API server with the Pod's service account. The
// token file is re-read per request: projected tokens rotate.
func inClusterAPI() (*k8sAPI, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("--state-secret needs to run inside Kubernetes (KUBERNETES_SERVICE_HOST unset)")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("service account ca.crt holds no certificate")
	}
	ns, err := os.ReadFile(saDir + "/namespace")
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &k8sAPI{
		base:      "https://" + net.JoinHostPort(host, port),
		namespace: strings.TrimSpace(string(ns)),
		client:    &http.Client{Transport: tr, Timeout: 10 * time.Second},
		token: func() (string, error) {
			b, err := os.ReadFile(saDir + "/token")
			return strings.TrimSpace(string(b)), err
		},
	}, nil
}

var errConflict = errors.New("state secret changed concurrently")

type k8sSecret struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   k8sMeta           `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Data       map[string][]byte `json:"data,omitempty"`
}

type k8sMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
}

func (a *k8sAPI) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return 0, err
	}
	tok, err := a.token()
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode/100 != 2 {
		var st struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &st)
		return resp.StatusCode, fmt.Errorf("%s %s: %s %s", method, path, resp.Status, st.Message)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(b, out)
	}
	return resp.StatusCode, nil
}

func (a *k8sAPI) secretsPath() string { return "/api/v1/namespaces/" + a.namespace + "/secrets" }

// getState returns the document and its resourceVersion ("" when the
// Secret does not exist: an empty document).
func (a *k8sAPI) getState(ctx context.Context, name string) (stateDoc, string, error) {
	var sec k8sSecret
	code, err := a.do(ctx, http.MethodGet, a.secretsPath()+"/"+name, nil, &sec)
	if code == http.StatusNotFound {
		return stateDoc{KV: map[string]string{}}, "", nil
	}
	if err != nil {
		return stateDoc{}, "", err
	}
	doc := stateDoc{KV: map[string]string{}}
	if raw := sec.Data[stateKey]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return stateDoc{}, "", fmt.Errorf("secret %s: %s: %w", name, stateKey, err)
		}
		if doc.KV == nil {
			doc.KV = map[string]string{}
		}
	}
	return doc, sec.Metadata.ResourceVersion, nil
}

// putState writes doc: an update guarded by rv, or a create when rv is "".
// errConflict means someone else wrote (or created) it first.
func (a *k8sAPI) putState(ctx context.Context, name string, doc stateDoc, rv string) (string, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	sec := k8sSecret{APIVersion: "v1", Kind: "Secret", Type: "Opaque",
		Metadata: k8sMeta{Name: name, Namespace: a.namespace, ResourceVersion: rv,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "ccdash-hub"}},
		Data: map[string][]byte{stateKey: raw}}
	var out k8sSecret
	var code int
	if rv == "" {
		code, err = a.do(ctx, http.MethodPost, a.secretsPath(), sec, &out)
	} else {
		code, err = a.do(ctx, http.MethodPut, a.secretsPath()+"/"+name, sec, &out)
	}
	if code == http.StatusConflict {
		return "", errConflict
	}
	if err != nil {
		return "", err
	}
	return out.Metadata.ResourceVersion, nil
}
