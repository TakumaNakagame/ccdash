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
	"regexp"
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
	if err != nil || len(subs) == 0 {
		return
	}
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

type watchState struct {
	first     bool
	status    map[string]model.SessionStatus
	lastSeen  map[string]time.Time
	approvals map[int64]bool
	prompts   map[string]string // pty key → prompt signature
	lastSent  map[string]time.Time
}

// watchDevice polls one connected device until ctx ends (the tunnel closed).
func (h *Hub) watchDevice(ctx context.Context, d Device, dc *deviceConn) {
	client := &http.Client{Transport: dc.transport, Timeout: 10 * time.Second}
	st := newWatchState()
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	for {
		ns, err := watchOnce(ctx, client, d, st)
		if err != nil && ctx.Err() == nil {
			log.Printf("hub: watch %s: %v", d.Name, err)
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

// watchOnce takes one look at a device and returns the notifications due.
// The first look only records a baseline.
func watchOnce(ctx context.Context, c *http.Client, d Device, st *watchState) ([]notification, error) {
	var sessions []model.Session
	if err := getJSON(ctx, c, "/api/sessions", &sessions); err != nil {
		return nil, err
	}
	var approvals []model.Approval
	_ = getJSON(ctx, c, "/api/approvals", &approvals) // 403 when approvals are off
	var ptys []struct {
		Key   string `json:"key"`
		Alive bool   `json:"alive"`
		PID   int    `json:"pid"`
	}
	_ = getJSON(ctx, c, "/pty/", &ptys)

	byID := map[string]model.Session{}
	for _, s := range sessions {
		byID[s.SessionID] = s
	}
	label := func(sid string) string {
		s, ok := byID[sid]
		if !ok {
			return "新しいセッション"
		}
		t := s.DisplayTitle()
		if r := []rune(t); len(r) > 60 {
			t = string(r[:60]) + "…"
		}
		if s.Num > 0 {
			return fmt.Sprintf("#%d %s", s.Num, t)
		}
		return t
	}
	var out []notification
	send := func(sid, key, title, body string) {
		if st.first {
			return
		}
		if time.Since(st.lastSent[key]) < sessionCooldown {
			return
		}
		st.lastSent[key] = time.Now()
		u := "/#/d/" + d.ID
		if sid != "" {
			u += "/s/" + url.PathEscape(sid)
		}
		out = append(out, notification{Title: d.Name + " · " + title, Body: body, URL: u, Tag: d.ID + ":" + key})
	}

	// New approvals.
	live := map[int64]bool{}
	for _, a := range approvals {
		if a.Status != model.ApprovalPending {
			continue
		}
		live[a.ID] = true
		if !st.approvals[a.ID] {
			send(a.SessionID, a.SessionID, "承認待ち", fmt.Sprintf("%s — %s", a.Tool, label(a.SessionID)))
		}
	}
	st.approvals = live

	// Turns that finished. A short turn can start and end between two
	// polls, so "was active, now isn't" misses it; a session that is idle
	// (alive, waiting for input) and whose last activity moved since the
	// previous look has finished a turn too — including a brand-new session
	// finishing its first one.
	for _, s := range sessions {
		prev, seen := st.status[s.SessionID]
		prevSeen := st.lastSeen[s.SessionID]
		switch {
		case seen && prev == model.StatusActive && s.Status != model.StatusActive:
			send(s.SessionID, s.SessionID+":done", "完了", label(s.SessionID))
		case s.Status == model.StatusIdle && s.LastSeen.After(prevSeen) && (seen || time.Since(s.LastSeen) < 2*watchInterval+10*time.Second):
			send(s.SessionID, s.SessionID+":done", "完了", label(s.SessionID))
		}
		st.status[s.SessionID] = s.Status
		st.lastSeen[s.SessionID] = s.LastSeen
	}

	// Questions / confirmations on hosted claudes' screens. One key per
	// process (a spawn is listed under pid-N and later its session id too).
	keyOf := map[int]string{}
	for _, p := range ptys {
		if !p.Alive {
			continue
		}
		if k, ok := keyOf[p.PID]; !ok || strings.HasPrefix(k, "pid-") {
			keyOf[p.PID] = p.Key
		}
	}
	prompts := map[string]string{}
	for _, key := range keyOf {
		var scr struct {
			Rows []string `json:"rows"`
		}
		if err := getJSON(ctx, c, "/pty/"+url.PathEscape(key)+"/text", &scr); err != nil {
			continue
		}
		kind, text := screenPromptKind(scr.Rows)
		if kind == "" {
			continue
		}
		prompts[key] = kind + ":" + text
		if st.prompts[key] != prompts[key] {
			sid := ""
			if !strings.HasPrefix(key, "pid-") {
				sid = key
			}
			title := "確認が必要です"
			if kind == "question" {
				title = "Claude からの質問"
			}
			body := text
			if sid != "" {
				body = label(sid) + "\n" + text
			}
			send(sid, key, title, body)
		}
	}
	st.prompts = prompts
	st.first = false
	return out, nil
}

func newWatchState() *watchState {
	return &watchState{first: true, status: map[string]model.SessionStatus{}, lastSeen: map[string]time.Time{}, approvals: map[int64]bool{}, prompts: map[string]string{}, lastSent: map[string]time.Time{}}
}

var (
	numberedCursor = regexp.MustCompile(`^\s*❯\s*\d{1,2}[.)]\s`)
	askTabs        = regexp.MustCompile(`[☐☒]\s*\S`)
)

// screenPromptKind spots what the portal's chat shows as cards (see
// screenAsk / screenPrompt in web/app.js): Claude Code's AskUserQuestion
// dialog ("question") or a numbered confirmation menu — permission, plan
// approval, folder trust ("confirm"). text is the question line.
func screenPromptKind(rows []string) (kind, text string) {
	all := strings.Join(rows, "\n")
	if strings.Contains(all, "Enter to select") && strings.Contains(all, "Chat about this") {
		for i, r := range rows {
			if askTabs.MatchString(r) {
				for _, q := range rows[i+1:] {
					if t := strings.TrimSpace(q); t != "" {
						return "question", t
					}
				}
			}
		}
		return "question", ""
	}
	unbox := func(r string) string { return strings.TrimSpace(strings.Trim(strings.TrimSpace(r), "│|")) }
	for i := len(rows) - 1; i >= 0; i-- {
		if !numberedCursor.MatchString(unbox(rows[i])) {
			continue
		}
		for j := i - 1; j >= 0 && j >= i-12; j-- {
			t := unbox(rows[j])
			if strings.HasSuffix(t, "?") || strings.HasSuffix(t, "？") {
				return "confirm", t
			}
		}
		return "confirm", ""
	}
	// Unnumbered list (the folder-trust question, the theme picker): the
	// cursor row has siblings in the column after the "❯". A lone "❯" row
	// is claude's input prompt, not a menu.
	// Only the bottom-most "❯" row counts: higher ones are past prompts in
	// the scrollback, and an empty one is the input box (no dialog open).
	for i := len(rows) - 1; i >= 0; i-- {
		r := unbox(rows[i])
		col := strings.Index(r, "❯")
		if col < 0 {
			continue
		}
		if strings.TrimSpace(r[col+len("❯"):]) == "" {
			return "", ""
		}
		raw := rows[i]
		c := strings.Index(raw, "❯")
		sib := func(l string) bool {
			return len(l) > c+len("❯") && strings.TrimSpace(l[:c]) == "" && strings.TrimSpace(l[c:]) != "" && !strings.Contains(l, "─")
		}
		n := 0
		for j := i - 1; j >= 0 && sib(rows[j]); j-- {
			n++
		}
		for j := i + 1; j < len(rows) && sib(rows[j]); j++ {
			n++
		}
		if n == 0 {
			return "", ""
		}
		for j := i - 1; j >= 0 && j >= i-12; j-- {
			if t := unbox(rows[j]); strings.HasSuffix(t, "?") || strings.HasSuffix(t, "？") || strings.Contains(t, "?") {
				return "confirm", t
			}
		}
		return "confirm", ""
	}
	return "", ""
}
