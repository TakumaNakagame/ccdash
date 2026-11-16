package server

// Messages between sessions (docs/decisions/0009-orchestrator.md §4, §13).
//
// A session reaches a running session of its own machine with Claude
// Code's SendMessage; ccdash only supplies the address. This file is for
// the ones SendMessage can't reach: a child (or parent) on another
// machine, or one whose claude has stopped. ccdash mcp's tell_session
// posts the text to the collector that has the target session, and that
// collector — not the sender — decides how it reads and when it arrives:
//
//   - It writes the first line itself (model.MessageHeader): who the
//     message is from, and that it is neither the operator's input nor an
//     approval. Through the hub the sending machine is the one the hub
//     authenticated (tunnel.HeaderPeerID …), never a field of the body.
//   - The two sessions must be parent and child (sessions.parent).
//   - It is queued in SQLite (the messages table) and handed to claude
//     only when that can't be taken for something else: never while an
//     approval or a dialog is up (the Enter would answer it), never
//     mid-turn, one message at a time. A stopped session ccdash can resume
//     is resumed with the message as its prompt; one running outside
//     ccdash keeps it queued until the operator continues it.
//
// The portal has no way to send one (hubAllowed), and nothing here answers
// an approval or a question.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/TakumaNakagame/ccdash/internal/hubcfg"
	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/plaintext"
	"github.com/TakumaNakagame/ccdash/internal/redact"
	"github.com/TakumaNakagame/ccdash/internal/settings"
	"github.com/TakumaNakagame/ccdash/internal/store"
	"github.com/TakumaNakagame/ccdash/internal/tunnel"
)

// Vars so tests can run the inbox fast.
var (
	// messageTick is how often the inbox is looked through.
	messageTick = 2 * time.Second
	// messageGap is the least time between two messages typed into one
	// session: the first has to become a turn (its prompt hook makes the
	// session active) before the next is considered.
	messageGap = 5 * time.Second
	// messageBoot: a claude started less than this ago is still coming up
	// and would lose what is typed at it.
	messageBoot = 8 * time.Second
	// messageHold: after a resume carried a message, nothing more is typed
	// until that turn has ended — or this long has passed without a sign
	// of it (hooks not installed).
	messageHold = 2 * time.Minute
	// messageRetry: how long a failed resume waits before the next try.
	messageRetry = time.Minute
)

// messageClip caps a message's text in GET …/brief?messages=1 (bytes).
const messageClip = 4000

// viaTunnelKey marks a request that reached the mux through the hub tunnel
// or the local portal's link (tunnelHandler) rather than with the loopback
// token.
type viaTunnelKey struct{}

func viaTunnel(r *http.Request) bool {
	v, _ := r.Context().Value(viaTunnelKey{}).(bool)
	return v
}

// messageReq is the body of POST /api/sessions/{id}/message. FromHost is
// accepted for older / other callers but never used: the sending machine
// is this one (a local call) or the device the hub names.
type messageReq struct {
	Text        string `json:"text"`
	FromSession string `json:"from_session"`
	FromRef     string `json:"from_ref"`
	FromHost    string `json:"from_host"`
}

// handleAPIMessage takes a message for session {id} from its parent or
// child, queues it and tries to deliver it at once.
// POST /api/sessions/{id}/message {"text","from_session","from_ref"}
// → {"state": "delivered" | "resumed" | "queued", "id", "note"}
func (s *Server) handleAPIMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	var req messageReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	// It types into / starts claude: the same switch as every other way.
	if cfg, err := settings.Load(ctx, s.db); err == nil && !cfg.AttachEnabled {
		http.Error(w, "attach is OFF on this device", http.StatusForbidden)
		return
	}
	text, err := plaintext.Check(req.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.FromSession == "" || !validParent(req.FromSession) {
		http.Error(w, "from_session (the sending session's id) is required", http.StatusBadRequest)
		return
	}
	target, ok, err := s.db.GetSession(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	host, ref, code, err := s.messageSender(r, target, req)
	if err != nil {
		log.Printf("message for %s refused: %v", id, err)
		http.Error(w, err.Error(), code)
		return
	}
	mid, err := s.db.InsertMessage(ctx, model.Message{
		SessionID: id, FromSession: req.FromSession, FromRef: ref, FromHost: host, Text: redact.String(text),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	got, how, why := s.deliverNext(ctx, id)
	out := map[string]any{"id": mid, "state": model.MessageQueued}
	switch {
	case got == mid:
		out["state"] = how
	case got != 0:
		out["note"] = "an earlier message to this session went first; this one follows once the session is idle again"
	default:
		out["note"] = why
	}
	writeOK(w, out)
}

// messageSender establishes who a message is from — the name of the
// sending machine ("" = this one) and the sender's ref for the header —
// or refuses it (403): the sender must be the target's parent or child.
//
// A local call (loopback token): the sender is a session of this
// collector, so its row decides everything; nothing but the session id is
// taken from the request. Through the hub: the machine is the device the
// hub authenticated; a parent must be recorded on the target as living on
// that device, a child is confirmed with that device through the hub. The
// ref's number is the one thing taken from the body there (the target
// can't know another machine's numbering); its letter is the hub's.
func (s *Server) messageSender(r *http.Request, target model.Session, req messageReq) (host, ref string, code int, err error) {
	ctx := r.Context()
	from := req.FromSession
	if from == target.SessionID {
		return "", "", http.StatusBadRequest, fmt.Errorf("a session can't send itself a message")
	}
	if !viaTunnel(r) {
		sx, ok, err := s.db.GetSession(ctx, from)
		if err != nil {
			return "", "", http.StatusInternalServerError, err
		}
		if !ok {
			return "", "", http.StatusForbidden, fmt.Errorf("from_session is not a session of this machine")
		}
		if target.Parent != from && sx.Parent != target.SessionID {
			return "", "", http.StatusForbidden, fmt.Errorf("the sender is neither this session's parent nor its child")
		}
		ref = hubcfg.WithLetter([]model.Session{sx})[0].Ref()
		if ref == "" {
			ref = shortID(from)
		}
		return "", ref, 0, nil
	}
	devID := r.Header.Get(tunnel.HeaderPeerID)
	if devID == "" {
		return "", "", http.StatusForbidden, fmt.Errorf("the hub did not name the sending device")
	}
	name, _ := url.QueryUnescape(r.Header.Get(tunnel.HeaderPeerName))
	name = cleanName(name)
	letter := r.Header.Get(tunnel.HeaderPeerLetter)
	if !peerLetter.MatchString(letter) {
		letter = ""
	}
	if target.Parent == from {
		// The parent's machine was recorded with it (start_session /
		// adopt_session write the device id, else the letter).
		if target.ParentHost == "" || target.ParentHost != devID && (letter == "" || !strings.EqualFold(target.ParentHost, letter)) {
			return "", "", http.StatusForbidden, fmt.Errorf("this session's parent is not a session of the sending device")
		}
	} else {
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		ok, err := s.childOnPeer(cctx, devID, from, target.SessionID)
		cancel()
		if err != nil {
			return "", "", http.StatusForbidden, fmt.Errorf("can't confirm with the sending device that the sender is this session's child (this machine needs peer access to it on the hub): %v", err)
		}
		if !ok {
			return "", "", http.StatusForbidden, fmt.Errorf("the sender is neither this session's parent nor its child")
		}
	}
	if name == "" {
		name = letter
	}
	if name == "" {
		name = "別の機械"
	}
	ref = shortID(from)
	if m := refNumber.FindStringSubmatch(strings.TrimSpace(req.FromRef)); m != nil {
		ref = "#" + m[1]
		if letter != "" {
			ref = strings.ToUpper(letter) + m[1]
		}
	}
	return name, ref, 0, nil
}

var (
	peerLetter = regexp.MustCompile(`^[A-Za-z]{1,3}$`)
	refNumber  = regexp.MustCompile(`^[A-Za-z]{0,3}#?([0-9]{1,9})$`)
)

// shortID names a session with no usable ref by the start of its id.
func shortID(id string) string {
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

// cleanName makes a device name fit for the header line: one line, no
// control characters, not long.
func cleanName(n string) string {
	n = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, n)
	n = strings.Join(strings.Fields(n), " ")
	if r := []rune(n); len(r) > 48 {
		n = string(r[:48])
	}
	return n
}

// hubChildOf asks the device deviceID, through the hub and with this
// machine's own device token, whether its session child has parent as its
// parent (Server.childOnPeer). The hub lets that through only while this
// machine has peer access.
func hubChildOf(ctx context.Context, deviceID, child, parent string) (bool, error) {
	cfg, err := hubcfg.Load()
	if err != nil {
		return false, err
	}
	st := store.NewHubPeer(strings.TrimRight(cfg.URL, "/")+"/api/d/"+url.PathEscape(deviceID), cfg.Token)
	for _, archived := range []bool{false, true} {
		ss, err := st.ListSessions(ctx, archived)
		if err != nil {
			return false, err
		}
		for _, x := range ss {
			if x.SessionID == child {
				return x.Parent == parent, nil
			}
		}
	}
	return false, nil
}

// messageText is what claude reads: the collector's header line, a blank
// line, the sender's text.
func messageText(m model.Message) string {
	return model.MessageHeader(m.FromHost, m.FromRef) + "\n\n" + m.Text
}

// messageLoop delivers what is queued, whenever the sessions allow it.
func (s *Server) messageLoop(ctx context.Context) {
	t := time.NewTicker(messageTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.deliverMessages(ctx)
	}
}

// deliverMessages tries the oldest queued message of every session that
// has one.
func (s *Server) deliverMessages(ctx context.Context) {
	ms, err := s.db.PendingMessages(ctx)
	if err != nil {
		log.Printf("messages: %v", err)
		return
	}
	tried := map[string]bool{}
	for _, m := range ms {
		if tried[m.SessionID] {
			continue
		}
		tried[m.SessionID] = true
		s.deliverNext(ctx, m.SessionID)
	}
}

// deliverNext hands the oldest queued message of session sid to its claude
// if that is safe right now. It returns the id of the message delivered
// and how (model.MessageTyped / MessageResumed), or 0 and why it waits.
func (s *Server) deliverNext(ctx context.Context, sid string) (id int64, how, why string) {
	s.msgMu.Lock()
	defer s.msgMu.Unlock()
	ms, err := s.db.PendingMessages(ctx)
	if err != nil {
		return 0, "", err.Error()
	}
	var m *model.Message
	for i := range ms {
		if ms[i].SessionID == sid {
			m = &ms[i]
			break
		}
	}
	if m == nil {
		return 0, "", ""
	}
	if cfg, err := settings.Load(ctx, s.db); err == nil && !cfg.AttachEnabled {
		return 0, "", "attach is OFF on this device; the message waits until it is turned on"
	}
	x, ok, err := s.db.GetSession(ctx, sid)
	if err != nil || !ok {
		return 0, "", "the session is not known to this collector any more"
	}
	s.ptyMu.Lock()
	e := s.ptyMap[sid]
	s.ptyMu.Unlock()
	now := time.Now()
	if e != nil && e.sess.Alive() {
		if why := s.typeBlocked(x, e, now); why != "" {
			return 0, "", why
		}
		if !e.typeInto("", messageText(*m), true, true) {
			return 0, "", "claude exited just now; the message is delivered when the session is resumed"
		}
		if err := s.db.MarkMessageDelivered(ctx, m.ID, model.MessageTyped); err != nil {
			log.Printf("message %d: %v", m.ID, err)
		}
		s.msgLast[sid] = now
		log.Printf("message %d delivered to %s (typed)", m.ID, sid)
		return m.ID, model.MessageTyped, ""
	}
	// Not hosted. A claude that runs somewhere else can't be typed into,
	// and a second one on the same transcript would garble it.
	if x.Status == model.StatusActive || x.Status == model.StatusIdle {
		return 0, "", "the session runs outside ccdash (in a terminal), so ccdash can't hand it the message; it stays queued until the operator continues the session under ccdash or it stops"
	}
	if x.Cwd == "" {
		return 0, "", "the session is stopped and has no known directory, so ccdash can't resume it; the message stays queued"
	}
	if t, ok := s.msgRetry[sid]; ok && now.Before(t) {
		return 0, "", "resuming the session failed a moment ago; ccdash tries again shortly"
	}
	if x.Archived {
		if err := s.db.SetArchived(ctx, sid, false); err != nil {
			log.Printf("message %d: unarchive %s: %v", m.ID, sid, err)
		}
	}
	if _, _, _, err := s.startPTY(ctx, ptyStartReq{SessionID: sid, ResumeID: sid, Cwd: x.Cwd, Prompt: messageText(*m)}); err != nil {
		s.msgRetry[sid] = now.Add(messageRetry)
		log.Printf("message %d: resume %s: %v", m.ID, sid, err)
		return 0, "", "the session is stopped and resuming it failed (" + err.Error() + "); ccdash tries again"
	}
	delete(s.msgRetry, sid)
	if err := s.db.MarkMessageDelivered(ctx, m.ID, model.MessageResumed); err != nil {
		log.Printf("message %d: %v", m.ID, err)
	}
	s.msgLast[sid], s.msgHold[sid] = now, now
	log.Printf("message %d delivered to %s (resumed)", m.ID, sid)
	return m.ID, model.MessageResumed, ""
}

// typeBlocked says why a message can't be typed into the hosted claude of
// x right now ("" = it can). The first three are the rule that matters: a
// message must never land on a prompt — an approval, a question, a menu —
// where its Enter would be the answer. Caller holds msgMu.
func (s *Server) typeBlocked(x model.Session, e *ptyEntry, now time.Time) string {
	sid := x.SessionID
	if x.PendingCount > 0 {
		return "the session is waiting for the operator to answer an approval; the message is delivered after that"
	}
	if why := screenReason(e); why != "" {
		return "the session shows a dialog for the operator (" + why + "); the message is delivered after that"
	}
	// A question / plan dialog announced by its hook, or a permission
	// notification — not a declared answer / action wait, which is an
	// ordinary idle prompt.
	if x.Attention == model.AttentionNeedsYou && !model.WaitNeedsYou(x.Wait) {
		return "the session is waiting for the operator (" + x.AttentionReason + "); the message is delivered after that"
	}
	if x.Status == model.StatusActive {
		return "the session is mid-turn; the message is delivered when the turn ends"
	}
	if now.Sub(e.started) < messageBoot {
		return "claude is still starting; the message is delivered in a moment"
	}
	if hold, ok := s.msgHold[sid]; ok {
		turnOver := !x.LastActivity.Before(hold.Truncate(time.Second)) && x.Status != model.StatusActive
		if !turnOver && now.Sub(hold) < messageHold {
			return "the session was just resumed with an earlier message; this one follows when that turn ends"
		}
		delete(s.msgHold, sid)
	}
	if last, ok := s.msgLast[sid]; ok && now.Sub(last) < messageGap {
		return "an earlier message was delivered a moment ago; this one follows when the session is idle again"
	}
	return ""
}
