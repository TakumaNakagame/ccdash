package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/TakumaNakagame/ccdash/internal/model"
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

func (s *store) subByEndpoint(ctx context.Context, endpoint string) (pushSub, error) {
	var p pushSub
	err := s.db.QueryRowContext(ctx, `SELECT id, email, endpoint, p256dh, auth FROM push_subs WHERE endpoint = ?`, endpoint).
		Scan(&p.ID, &p.Email, &p.Endpoint, &p.P256dh, &p.Auth)
	return p, err
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
	// Badge is the app-icon count at send time (needs you + unread), so the
	// installed app's badge moves even while no portal window is open.
	Badge *int `json:"badge,omitempty"`
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
	for _, s := range subs {
		_, _ = h.sendPush(ctx, s, n)
	}
}

// sendPush sends n to one subscription and returns the push service's
// HTTP status (0 when the request never got an answer). A subscription the
// service says is gone is dropped.
func (h *Hub) sendPush(ctx context.Context, s pushSub, n notification) (int, error) {
	pub, priv, err := h.vapidKeys(ctx)
	if err != nil {
		log.Printf("hub: push: %v", err)
		return 0, err
	}
	payload, _ := json.Marshal(n)
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: s.Endpoint,
		Keys:     webpush.Keys{P256dh: s.P256dh, Auth: s.Auth},
	}, &webpush.Options{
		HTTPClient:      h.pushClient,
		Subscriber:      h.pushSubject(),
		VAPIDPublicKey:  pub,
		VAPIDPrivateKey: priv,
		TTL:             600,
		Urgency:         webpush.UrgencyHigh,
	})
	if err != nil {
		log.Printf("hub: push to %s: %v", s.Email, err)
		return 0, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		_ = h.st.removeSub(ctx, s.Endpoint)
		log.Printf("hub: dropped expired push subscription of %s", s.Email)
		return resp.StatusCode, fmt.Errorf("the push service says this subscription is gone (HTTP %d)", resp.StatusCode)
	case resp.StatusCode >= 300:
		log.Printf("hub: push to %s: HTTP %d %s", s.Email, resp.StatusCode, strings.TrimSpace(string(body)))
		return resp.StatusCode, fmt.Errorf("push service answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.StatusCode, nil
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

// handlePushTest sends a test notification. With {"endpoint"} (the
// portal's diagnostic) it goes to that browser only and the answer says
// what the push service did, so the operator can tell a hub → push service
// problem (proxy, firewall, rejected VAPID) from a browser / OS one.
//
// {"dummy": true} sends one shaped like a real "needs you" notification
// with a tag of its own (so it never silently replaces an earlier one),
// and {"delay": N} (≤ 120 s) sends it later, so the operator can switch
// to another app — or lock the screen — and see what a real one does.
func (h *Hub) handlePushTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
		Dummy    bool   `json:"dummy"`
		Delay    int    `json:"delay"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req)
	n := notification{Title: "ccdash", Body: "通知のテストです", URL: "/", Tag: "test"}
	if req.Dummy {
		n = notification{Title: "ccdash · 要対応（ダミー）", Body: "通知のテストです。実際の通知と同じ経路・同じ形で送っています", URL: "/", Tag: "dummy-" + randHex(4)}
	}
	if req.Endpoint == "" {
		h.push(r.Context(), n)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sub, err := h.st.subByEndpoint(r.Context(), req.Endpoint)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "this browser's subscription is not registered here", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]any{"service": ""}
	if u, err := url.Parse(sub.Endpoint); err == nil {
		out["service"] = u.Host
	}
	if req.Delay > 0 {
		delay := time.Duration(min(req.Delay, 120)) * time.Second
		go func() {
			time.Sleep(delay)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := h.sendPush(ctx, sub, n); err == nil {
				log.Printf("hub: sent the delayed test notification")
			}
		}()
		out["scheduled"] = int(delay.Seconds())
		writeJSON(w, out)
		return
	}
	status, err := h.sendPush(r.Context(), sub, n)
	out["status"] = status
	out["ok"] = err == nil
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, out)
}

// ---- device watcher ----

// The collector decides what each session wants from the operator
// (model.Session.Attention: "needs_you" / "done"; see internal/server
// attention.go), so the hub only watches that field change. Each look also
// refreshes the device's snapshot for the cross-device board.

type watchState struct {
	first       bool
	restart     string            // pending / last outcome of the device's scheduled restart, last look
	restartSeen bool              // restart holds a baseline
	attention   map[string]string // session id → attention|reason last seen
	lastSent    map[string]time.Time
}

func newWatchState() *watchState {
	return &watchState{first: true, attention: map[string]string{}, lastSent: map[string]time.Time{}}
}

// watchDevice polls one connected device until ctx ends (the tunnel closed).
func (h *Hub) watchDevice(ctx context.Context, d Device, dc *deviceConn) {
	client := &http.Client{Transport: dc.rt, Timeout: 10 * time.Second}
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
			// A collector older than scheduled restarts answers 404: no state.
			var rs *model.RestartState
			var got model.RestartState
			if getJSON(ctx, client, "/api/restart", &got) == nil {
				rs = &got
			}
			if n, ok := restartNotice(d, rs, st); ok {
				ns = append(ns, n)
			}
			h.storeSnapshot(d, sessions, rs)
		}
		for _, n := range ns {
			b := h.badgeCount()
			n.Badge = &b
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
	if ref := s.Ref(); ref != "" {
		return ref + " " + t
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
	for i := range sessions { // an older collector doesn't stamp its letter
		if sessions[i].HostLetter == "" {
			sessions[i].HostLetter = d.Letter
		}
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
			if l := model.WaitLabel(s.Wait); model.WaitNeedsYou(s.Wait) && strings.HasPrefix(reason, l) {
				// Declared with set_status: "回答待ち" / "作業待ち" says more.
				n.Title = d.Name + " · " + l
				reason = strings.TrimPrefix(strings.TrimPrefix(reason, l), ": ")
			}
			if reason != "" {
				n.Body += "\n" + reason
			}
		case model.AttentionDone:
			// What the session declared it hands back (set_status), if anything.
			label := model.WaitLabel(s.Wait)
			if label == "" {
				label = "完了"
			}
			n.Title = d.Name + " · " + label
			n.Body = sessionLabel(s)
			if s.Wait != "" && s.WaitNote != "" {
				n.Body += "\n" + s.WaitNote
			}
		default:
			continue
		}
		out = append(out, n)
	}
	out = append(out, spawnPrompts(ctx, c, d, st, now)...)
	st.attention = now
	st.first = false
	return out, sessions, nil
}

// spawnPrompts notifies about claudes started on the device that wait on a
// dialog before they have a session at all (the folder-trust question in a
// new folder): no session row carries their attention, so GET /pty does.
// now gets "pty:<key>" signatures, deduped like sessions.
func spawnPrompts(ctx context.Context, c *http.Client, d Device, st *watchState, now map[string]string) []notification {
	var ptys []struct {
		Key    string `json:"key"`
		Alive  bool   `json:"alive"`
		PID    int    `json:"pid"`
		Cwd    string `json:"cwd"`
		Prompt string `json:"prompt"`
	}
	if getJSON(ctx, c, "/pty/", &ptys) != nil {
		return nil
	}
	named := map[int]bool{}
	for _, p := range ptys {
		if !strings.HasPrefix(p.Key, "pid-") {
			named[p.PID] = true
		}
	}
	var out []notification
	for _, p := range ptys {
		if !p.Alive || p.Prompt == "" || named[p.PID] || !strings.HasPrefix(p.Key, "pid-") {
			continue
		}
		sk := "pty:" + p.Key
		now[sk] = p.Prompt
		if st.first || st.attention[sk] == p.Prompt || time.Since(st.lastSent[sk]) < sessionCooldown {
			continue
		}
		st.lastSent[sk] = time.Now()
		label := "新しいセッション"
		if p.Cwd != "" {
			label += "（" + filepath.Base(p.Cwd) + "）"
		}
		out = append(out, notification{
			Title: d.Name + " · 要対応",
			Body:  label + "\n" + p.Prompt,
			URL:   "/#/d/" + d.ID + "/p/" + url.PathEscape(p.Key),
			Tag:   d.ID + ":" + p.Key,
		})
	}
	return out
}

// ---- cross-device board ----

type deviceSnapshot struct {
	Device   Device
	Sessions []model.Session
	Restart  *model.RestartState // nil: the collector predates scheduled restarts
	At       time.Time
}

func (h *Hub) storeSnapshot(d Device, sessions []model.Session, rs *model.RestartState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.snapshots == nil {
		h.snapshots = map[string]deviceSnapshot{}
	}
	// A collector that predates letters doesn't stamp its rows: the hub
	// knows the device's letter.
	for i := range sessions {
		if sessions[i].HostLetter == "" {
			sessions[i].HostLetter = d.Letter
		}
	}
	h.snapshots[d.ID] = deviceSnapshot{Device: d, Sessions: sessions, Restart: rs, At: time.Now()}
}

// restartNotice pushes a warning when a device's collector schedules a
// restart (its hosted sessions will be stopped and resumed) and when a
// schedule gives up. The first look is a baseline, like attention.
func restartNotice(d Device, rs *model.RestartState, st *watchState) (notification, bool) {
	sig := ""
	if rs != nil {
		switch {
		case rs.Pending:
			sig = "pending"
		case rs.Last != "":
			sig = rs.Last + "@" + rs.LastAt.String()
		}
	}
	prev, seen := st.restart, st.restartSeen
	st.restart, st.restartSeen = sig, true
	if !seen || sig == prev || sig == "" {
		return notification{}, false
	}
	n := notification{URL: "/#/d/" + d.ID, Tag: d.ID + ":restart"}
	switch {
	case rs.Pending:
		n.Title = d.Name + " · 再起動を予約"
		n.Body = fmt.Sprintf("作業中のセッションが終わり次第 ccdash を再起動します（%d 件待ち）。ホスト中の %d 件はいったん止まり、自動で再開されます。", len(rs.Waiting), rs.Hosted)
	case rs.Last == "expired":
		n.Title = d.Name + " · 再起動の予約は時間切れ"
		n.Body = "作業中のセッションが終わらなかったため、ccdash の再起動を取りやめました。"
	default:
		return notification{}, false
	}
	return n, true
}

// allSnapshots is every device's latest snapshot: this replica's watchers
// plus the other replicas' (cluster.go).
func (h *Hub) allSnapshots() map[string]deviceSnapshot {
	out := map[string]deviceSnapshot{}
	h.mu.Lock()
	for id, snap := range h.snapshots {
		out[id] = snap
	}
	h.mu.Unlock()
	if h.cl != nil {
		for _, snap := range h.cl.snapshots() {
			if cur, ok := out[snap.Device.ID]; !ok || snap.At.After(cur.At) {
				out[snap.Device.ID] = snap
			}
		}
	}
	return out
}

// connInfo reports whether device id is connected to any replica.
func (h *Hub) connInfo(id string) (peerConn, bool) {
	if dc := h.localConn(id); dc != nil {
		return peerConn{ID: id, Since: dc.since, Remote: dc.remote}, true
	}
	if h.cl != nil {
		return h.cl.info(id)
	}
	return peerConn{}, false
}

// onlineIDs lists the devices connected to any replica.
func (h *Hub) onlineIDs() []string {
	var ids []string
	h.mu.Lock()
	for id := range h.conns {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	if h.cl != nil {
		for _, id := range h.cl.onlineIDs() {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return ids
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
	dev := h.locateSession(sid)
	if dev == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>ccdash hub</title>` +
			`<p style="font-family:sans-serif">このセッションを持つ端末が見つかりません（オフラインかもしれません）。 <a href="/">ポータルを開く</a></p>`))
		return
	}
	http.Redirect(w, r, "/#/d/"+url.PathEscape(dev)+"/s/"+url.PathEscape(sid), http.StatusFound)
}

// handleLocate is /s/<id> for the SPA: a session link tapped inside the
// portal moves to the chat in place instead of reloading.
func (h *Hub) handleLocate(w http.ResponseWriter, r *http.Request) {
	dev := h.locateSession(r.PathValue("session"))
	if dev == "" {
		http.Error(w, "no connected device has this session", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"device_id": dev})
}

// locateSession names the device that reports sid, or "".
func (h *Hub) locateSession(sid string) string {
	for id, snap := range h.allSnapshots() {
		for _, s := range snap.Sessions {
			if s.SessionID == sid {
				return id
			}
		}
	}
	if ids := h.onlineIDs(); len(ids) == 1 {
		return ids[0]
	}
	return ""
}

type boardCard struct {
	DeviceID   string        `json:"device_id"`
	DeviceName string        `json:"device_name"`
	Session    model.Session `json:"session"`
}

// boardColumn places a session on the board: it needs the operator, is
// working, or finished unread (attention done, or NEW: progress since it
// was last read) — or "" when it is none of those.
func boardColumn(s model.Session) string {
	switch {
	case s.Attention == model.AttentionNeedsYou:
		return "needs_you"
	case s.Status == model.StatusActive:
		return "working"
	case s.Attention == model.AttentionDone || s.Unseen:
		return "done"
	}
	return ""
}

// badgeCount is the app-icon badge: sessions that need the operator plus
// unread finishes, across every connected device (the portal's
// updateBadge counts the same from /api/board).
func (h *Hub) badgeCount() int {
	n := 0
	for _, snap := range h.allSnapshots() {
		for _, s := range snap.Sessions {
			if k := boardColumn(s); k == "needs_you" || k == "done" {
				n++
			}
		}
	}
	return n
}

// handleBoard is the portal's cross-device view, served from the watchers'
// latest snapshots: sessions that need the operator, are working, or
// finished unread — without a round trip to every device.
func (h *Hub) handleBoard(w http.ResponseWriter, r *http.Request) {
	board := map[string][]boardCard{"needs_you": {}, "working": {}, "done": {}}
	for _, snap := range h.allSnapshots() {
		for _, s := range snap.Sessions {
			c := boardCard{DeviceID: snap.Device.ID, DeviceName: snap.Device.Name, Session: s}
			if k := boardColumn(s); k != "" {
				board[k] = append(board[k], c)
			}
		}
	}
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
	for _, snap := range h.allSnapshots() {
		for _, s := range snap.Sessions {
			if all || s.Status == model.StatusActive || s.Status == model.StatusIdle || s.Attention == model.AttentionNeedsYou {
				out = append(out, boardCard{DeviceID: snap.Device.ID, DeviceName: snap.Device.Name, Session: s})
			}
		}
	}
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
