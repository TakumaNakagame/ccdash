package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/takumanakagame/ccmanage/internal/model"
)

// Web Push: the hub watches every connected device and notifies the
// browsers that subscribed (the installed PWA on a phone, typically) when
// something needs the operator — a new approval, a question or confirmation
// on a hosted claude's screen — or when a session finishes its turn.
//
// Watching is polling through the tunnel (the same device API the portal
// uses), so devices need nothing new for it.

const watchInterval = 4 * time.Second

// sessionCooldown keeps one session from buzzing twice in a row (e.g. a
// permission request that shows up both as an approval and on screen).
const sessionCooldown = 15 * time.Second

type pushSub struct {
	ID       int64
	Email    string
	Endpoint string
	P256dh   string
	Auth     string
}

func (s *store) addSub(ctx context.Context, email string, sub pushSub) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO push_subs (email, endpoint, p256dh, auth, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET email = excluded.email, p256dh = excluded.p256dh, auth = excluded.auth`,
		email, sub.Endpoint, sub.P256dh, sub.Auth, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *store) removeSub(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_subs WHERE endpoint = ?`, endpoint)
	return err
}

func (s *store) listSubs(ctx context.Context) ([]pushSub, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, email, endpoint, p256dh, auth FROM push_subs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pushSub
	for rows.Next() {
		var p pushSub
		if err := rows.Scan(&p.ID, &p.Email, &p.Endpoint, &p.P256dh, &p.Auth); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// notification is the payload the service worker turns into a system
// notification.
type notification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"` // a newer notification with the same tag replaces the older one
}

// vapidKeys returns the hub's VAPID key pair, generated on first use and
// stored as one kv row so the two halves can never mismatch.
func (h *Hub) vapidKeys(ctx context.Context) (pub, priv string, err error) {
	v, err := h.st.secret(ctx, "vapid", func() string {
		priv, pub, gerr := webpush.GenerateVAPIDKeys()
		if gerr != nil {
			return ""
		}
		return pub + " " + priv
	})
	if err != nil {
		return "", "", err
	}
	pub, priv, ok := strings.Cut(v, " ")
	if !ok || pub == "" || priv == "" {
		return "", "", errors.New("vapid key generation failed")
	}
	return pub, priv, nil
}

// push sends n to every subscription, dropping the ones the push service
// says are gone.
func (h *Hub) push(ctx context.Context, n notification) {
	subs, err := h.st.listSubs(ctx)
	if err != nil {
		log.Printf("hub: push %q: %v", n.Title, err)
		return
	}
	if len(subs) == 0 {
		log.Printf("hub: push %q: no subscriptions (turn on 🔔 in the portal)", n.Title)
		return
	}
	log.Printf("hub: push %q to %d subscription(s)", n.Title, len(subs))
	pub, priv, err := h.vapidKeys(ctx)
	if err != nil {
		log.Printf("hub: push: %v", err)
		return
	}
	payload, _ := json.Marshal(n)
	for _, s := range subs {
		resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
			Endpoint: s.Endpoint,
			Keys:     webpush.Keys{P256dh: s.P256dh, Auth: s.Auth},
		}, &webpush.Options{
			Subscriber:      h.cfg.PublicURL,
			VAPIDPublicKey:  pub,
			VAPIDPrivateKey: priv,
			TTL:             600,
			Urgency:         webpush.UrgencyHigh,
		})
		if err != nil {
			log.Printf("hub: push to %s: %v", s.Email, err)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusNotFound, http.StatusGone:
			_ = h.st.removeSub(ctx, s.Endpoint)
			log.Printf("hub: dropped expired push subscription of %s", s.Email)
		default:
			if resp.StatusCode >= 300 {
				log.Printf("hub: push to %s: HTTP %d", s.Email, resp.StatusCode)
			}
		}
	}
}

func (h *Hub) handlePushKey(w http.ResponseWriter, r *http.Request) {
	pub, _, err := h.vapidKeys(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"publicKey": pub})
}

func (h *Hub) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	var sub struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&sub); err != nil || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		http.Error(w, "bad subscription", http.StatusBadRequest)
		return
	}
	if u, err := url.Parse(sub.Endpoint); err != nil || u.Scheme != "https" {
		http.Error(w, "push endpoint must be https", http.StatusBadRequest)
		return
	}
	email, _ := h.auth.user(r)
	if err := h.st.addSub(r.Context(), email, pushSub{Endpoint: sub.Endpoint, P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if u, err := url.Parse(sub.Endpoint); err == nil {
		log.Printf("hub: %s subscribed to push (%s)", email, u.Host)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req); err != nil || req.Endpoint == "" {
		http.Error(w, "endpoint required", http.StatusBadRequest)
		return
	}
	if err := h.st.removeSub(r.Context(), req.Endpoint); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) handlePushTest(w http.ResponseWriter, r *http.Request) {
	h.push(r.Context(), notification{Title: "ccdash hub", Body: "通知のテストです", URL: "/", Tag: "test"})
	w.WriteHeader(http.StatusNoContent)
}

// ---- device watcher ----

// The collector decides what each session wants from the operator
// (model.Session.Attention: "needs_you" / "done"; see internal/server
// attention.go), so the hub only watches that field change. Each look also
// refreshes the device's snapshot for the cross-device board.

type watchState struct {
	first     bool
	attention map[string]string // session id → attention|reason last seen
	lastSent  map[string]time.Time
}

func newWatchState() *watchState {
	return &watchState{first: true, attention: map[string]string{}, lastSent: map[string]time.Time{}}
}

// watchDevice polls one connected device until ctx ends (the tunnel closed).
func (h *Hub) watchDevice(ctx context.Context, d Device, dc *deviceConn) {
	client := &http.Client{Transport: dc.transport, Timeout: 10 * time.Second}
	st := newWatchState()
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	defer h.dropSnapshot(d.ID)
	for {
		ns, sessions, err := watchOnce(ctx, client, d, st)
		if err != nil && ctx.Err() == nil {
			log.Printf("hub: watch %s: %v", d.Name, err)
		}
		if err == nil {
			h.storeSnapshot(d, sessions)
		}
		for _, n := range ns {
			h.push(ctx, n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func getJSON(ctx context.Context, c *http.Client, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://device"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func sessionLabel(s model.Session) string {
	t := s.DisplayTitle()
	if r := []rune(t); len(r) > 60 {
		t = string(r[:60]) + "…"
	}
	if s.Num > 0 {
		return fmt.Sprintf("#%d %s", s.Num, t)
	}
	return t
}

// watchOnce takes one look at a device and returns the notifications due
// plus the session list. The first look only records a baseline.
func watchOnce(ctx context.Context, c *http.Client, d Device, st *watchState) ([]notification, []model.Session, error) {
	var sessions []model.Session
	if err := getJSON(ctx, c, "/api/sessions", &sessions); err != nil {
		return nil, nil, err
	}
	// Approvals name the tool, which makes a better "needs you" line.
	var approvals []model.Approval
	_ = getJSON(ctx, c, "/api/approvals", &approvals) // 403 when approvals are off
	toolOf := map[string]string{}
	for _, a := range approvals {
		if a.Status == model.ApprovalPending {
			toolOf[a.SessionID] = a.Tool
		}
	}

	var out []notification
	now := map[string]string{}
	for _, s := range sessions {
		if s.Attention == "" {
			continue
		}
		reason := s.AttentionReason
		if tool := toolOf[s.SessionID]; tool != "" && reason == "承認待ち" {
			reason = "承認待ち: " + tool
		}
		sig := s.Attention + "|" + reason
		now[s.SessionID] = sig
		if st.first || st.attention[s.SessionID] == sig {
			continue
		}
		key := s.SessionID + ":" + s.Attention
		if time.Since(st.lastSent[key]) < sessionCooldown {
			continue
		}
		st.lastSent[key] = time.Now()
		n := notification{URL: "/#/d/" + d.ID + "/s/" + url.PathEscape(s.SessionID), Tag: d.ID + ":" + s.SessionID}
		switch s.Attention {
		case model.AttentionNeedsYou:
			n.Title = d.Name + " · 要対応"
			n.Body = sessionLabel(s)
			if reason != "" {
				n.Body += "\n" + reason
			}
		case model.AttentionDone:
			n.Title = d.Name + " · 完了"
			n.Body = sessionLabel(s)
		default:
			continue
		}
		out = append(out, n)
	}
	st.attention = now
	st.first = false
	return out, sessions, nil
}

// ---- cross-device board ----

type deviceSnapshot struct {
	Device   Device
	Sessions []model.Session
	At       time.Time
}

func (h *Hub) storeSnapshot(d Device, sessions []model.Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.snapshots == nil {
		h.snapshots = map[string]deviceSnapshot{}
	}
	h.snapshots[d.ID] = deviceSnapshot{Device: d, Sessions: sessions, At: time.Now()}
}

func (h *Hub) dropSnapshot(id string) {
	h.mu.Lock()
	delete(h.snapshots, id)
	h.mu.Unlock()
}

// handleSessionLink sends /s/<session id> to that session's chat on
// whichever device reports it. A session the snapshots don't list (e.g.
// archived) goes to the only connected device when there is just one —
// its chat loads any session by id.
func (h *Hub) handleSessionLink(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("session")
	dev := ""
	h.mu.Lock()
	for id, snap := range h.snapshots {
		for _, s := range snap.Sessions {
			if s.SessionID == sid {
				dev = id
			}
		}
	}
	if dev == "" && len(h.conns) == 1 {
		for id := range h.conns {
			dev = id
		}
	}
	h.mu.Unlock()
	if dev == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>ccdash hub</title>` +
			`<p style="font-family:sans-serif">このセッションを持つ端末が見つかりません（オフラインかもしれません）。 <a href="/">ポータルを開く</a></p>`))
		return
	}
	http.Redirect(w, r, "/#/d/"+url.PathEscape(dev)+"/s/"+url.PathEscape(sid), http.StatusFound)
}

type boardCard struct {
	DeviceID   string        `json:"device_id"`
	DeviceName string        `json:"device_name"`
	Session    model.Session `json:"session"`
}

// handleBoard is the portal's cross-device view, served from the watchers'
// latest snapshots: sessions that need the operator, are working, or
// finished unread — without a round trip to every device.
func (h *Hub) handleBoard(w http.ResponseWriter, r *http.Request) {
	board := map[string][]boardCard{"needs_you": {}, "working": {}, "done": {}}
	h.mu.Lock()
	for _, snap := range h.snapshots {
		for _, s := range snap.Sessions {
			c := boardCard{DeviceID: snap.Device.ID, DeviceName: snap.Device.Name, Session: s}
			switch {
			case s.Attention == model.AttentionNeedsYou:
				board["needs_you"] = append(board["needs_you"], c)
			case s.Status == model.StatusActive:
				board["working"] = append(board["working"], c)
			case s.Attention == model.AttentionDone:
				board["done"] = append(board["done"], c)
			}
		}
	}
	h.mu.Unlock()
	for _, k := range []string{"needs_you", "working", "done"} {
		cs := board[k]
		sort.Slice(cs, func(i, j int) bool { return cs[i].Session.LastSeen.After(cs[j].Session.LastSeen) })
	}
	writeJSON(w, board)
}

// handleActive lists, across connected devices, the sessions that are
// running (a live claude: active or idle) or want the operator — the grid
// view. Needs-you first, then working, then idle; newest first within.
// ?all=1 lists every session the devices report (the grid's "add" picker
// and tiles the operator placed on stopped sessions).
func (h *Hub) handleActive(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1"
	out := []boardCard{}
	h.mu.Lock()
	for _, snap := range h.snapshots {
		for _, s := range snap.Sessions {
			if all || s.Status == model.StatusActive || s.Status == model.StatusIdle || s.Attention == model.AttentionNeedsYou {
				out = append(out, boardCard{DeviceID: snap.Device.ID, DeviceName: snap.Device.Name, Session: s})
			}
		}
	}
	h.mu.Unlock()
	rank := func(s model.Session) int {
		switch {
		case s.Attention == model.AttentionNeedsYou:
			return 0
		case s.Status == model.StatusActive:
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Session, out[j].Session
		if rank(a) != rank(b) {
			return rank(a) < rank(b)
		}
		return a.LastSeen.After(b.LastSeen)
	})
	writeJSON(w, out)
}
