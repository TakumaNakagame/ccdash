// Package handoffmcp is `ccdash handoff-mcp` (formerly `grok-mcp`): a small
// MCP server over Streamable HTTP for an outside caller (an assistant through
// the operator's MCP gateway, or a bot) that may hand work to Claude but must
// not drive the operator's own sessions. Its four tools:
//
//   - start_session: a new claude in one fixed directory (the flag, never an
//     argument), in auto mode, marked with the instance's origin
//     (Config.Origin → model.Session.Origin)
//   - list_sessions: every session, but only ref / title / status / the
//     declared one-line note
//   - read_session: the conversation (user and assistant text, no tool
//     calls) of a session it started
//   - send_message: more text for a session it started (typed into the live
//     claude, or a claude --resume when it has stopped)
//
// It never answers approvals or dialogs, never reads or writes another
// session's conversation, and refuses text that Claude Code would treat as
// a command (/, !, #) or that carries control characters.
//
// It is stateless: every POST is one JSON-RPC message, answered as
// application/json (the Streamable HTTP transport without a session or SSE).
package handoffmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/store"
	"github.com/TakumaNakagame/ccdash/internal/transcript"
)

// DefaultOrigin marks the sessions it starts unless Config.Origin says otherwise.
// Each instance sees only the sessions carrying its own origin.
const DefaultOrigin = "handoff"

// maxText caps a request / message (runes).
const maxText = 8000

// Config is what the operator fixes on the command line.
type Config struct {
	Cwd     string // where start_session starts claude (absolute)
	Project string // ccdash project the new sessions join ("" = none)
	HubURL  string // hub origin for session links ("" = no links)
	Origin  string // marks the sessions it starts ("" = DefaultOrigin)
	// StartHint is appended to start_session's description: advice for this
	// caller on writing the request (the grok-mcp alias: speech-recognition fixes).
	StartHint string
}

// Server answers the tools over a store.
type Server struct {
	st      store.Store
	cfg     Config
	version string
	now     func() time.Time
	// startWait is how long start_session waits for the new session's row.
	startWait time.Duration
	tick      time.Duration
}

// New returns a server over st.
func New(st store.Store, cfg Config, version string) *Server {
	cfg.HubURL = strings.TrimRight(cfg.HubURL, "/")
	if cfg.Origin == "" {
		cfg.Origin = DefaultOrigin
	}
	return &Server{st: st, cfg: cfg, version: version, now: time.Now, startWait: 20 * time.Second, tick: time.Second}
}

const instructions = `ccdash runs Claude Code sessions on the operator's work machine.
Hand Claude a piece of work with start_session (one request in plain words; Claude works in auto
mode in a fixed inbox directory, whose own instructions say how far it goes). list_sessions shows every session briefly; read_session
shows the conversation of a session you started; send_message adds to a session you started
(a follow-up, a correction, "how is it going?"). Sessions you did not start are the
operator's: you can see their title and status only.`

func (s *Server) tools() []map[string]any {
	sessionArg := map[string]any{"type": "string", "description": `the session's ref as list_sessions shows it (e.g. "B231") or its session_id`}
	return []map[string]any{
		{
			"name": "start_session",
			"description": "Start a new Claude Code session (auto mode) on the operator's work machine with this request as its first message. " +
				"Write the whole request in plain words: what to look into or change, and what you already know. " + s.cfg.StartHint +
				"Claude proceeds on its own; how far it goes is set by the inbox directory's instructions. " +
				"Returns the session's ref (and a link when the machine is joined to a hub); follow it with read_session.",
			"inputSchema": object(map[string]any{
				"request": map[string]any{"type": "string", "description": "the request, in plain words (no leading / ! # or -)"},
			}, "request"),
			"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": true},
		},
		{
			"name":        "list_sessions",
			"description": "List the Claude Code sessions, newest first: ref, title, status, the one-line note the session left, and whether you started it.",
			"inputSchema": object(map[string]any{
				"started_by_me": map[string]any{"type": "boolean", "description": "only the sessions you started"},
				"limit":         map[string]any{"type": "integer", "description": "at most this many rows (default 20, max 50)"},
			}),
			"annotations": map[string]any{"readOnlyHint": true},
		},
		{
			"name":        "read_session",
			"description": "Read the recent conversation (the requests and Claude's replies, newest last) of a session you started, with its status.",
			"inputSchema": object(map[string]any{
				"session":  sessionArg,
				"messages": map[string]any{"type": "integer", "description": "how many recent messages (default 6, max 30)"},
			}, "session"),
			"annotations": map[string]any{"readOnlyHint": true},
		},
		{
			"name": "send_message",
			"description": "Send a follow-up message to a session you started: an extra request, a correction or a status question. " +
				"A stopped session is resumed with it. Refused while the session shows a permission prompt or dialog (the operator answers those).",
			"inputSchema": object(map[string]any{
				"session": sessionArg,
				"text":    map[string]any{"type": "string", "description": "the message (no leading / ! # or -)"},
			}, "session", "text"),
			"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": true},
		},
	}
}

func object(props map[string]any, req ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(req) > 0 {
		o["required"] = req
	}
	return o
}

// call runs one tool and returns its JSON text.
func (s *Server) call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var a struct {
		Request     string `json:"request"`
		Session     string `json:"session"`
		Text        string `json:"text"`
		StartedByMe bool   `json:"started_by_me"`
		Limit       int    `json:"limit"`
		Messages    int    `json:"messages"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields() // a cwd or a mode is not the caller's to give
		if err := dec.Decode(&a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	var v any
	var err error
	switch name {
	case "start_session":
		v, err = s.startSession(ctx, a.Request)
	case "list_sessions":
		v, err = s.listSessions(ctx, a.StartedByMe, a.Limit)
	case "read_session":
		v, err = s.readSession(ctx, a.Session, a.Messages)
	case "send_message":
		v, err = s.sendMessage(ctx, a.Session, a.Text)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(v, "", " ")
	return string(b), err
}

// checkText refuses what Claude Code's prompt would not take as plain text:
// a leading / (slash command), ! (shell), # (memory) or - (a flag when it
// is claude's argument), and control characters (ESC sequences such as
// shift+tab switch modes; only newline and tab pass).
func checkText(t string) (string, error) {
	t = strings.TrimSpace(t)
	if t == "" {
		return "", fmt.Errorf("text is empty")
	}
	if n := len([]rune(t)); n > maxText {
		return "", fmt.Errorf("text is too long (%d characters, max %d)", n, maxText)
	}
	if strings.ContainsAny(t[:1], "/!#-") {
		return "", fmt.Errorf("text must not start with %q (Claude Code would read it as a command); start with a word", t[:1])
	}
	for _, r := range t {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || r == ' ' || r == ' ') {
			return "", fmt.Errorf("text must not contain control characters")
		}
	}
	return t, nil
}

func (s *Server) startSession(ctx context.Context, request string) (any, error) {
	prompt, err := checkText(request)
	if err != nil {
		return nil, err
	}
	before, err := s.st.ListSessions(ctx, false)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, x := range before {
		known[x.SessionID] = true
	}
	key, err := s.st.StartSession(ctx, store.StartRequest{
		Cwd: s.cfg.Cwd, Prompt: prompt, Project: s.cfg.Project, PermissionMode: "auto", Origin: s.cfg.Origin,
	})
	if err != nil {
		return nil, fmt.Errorf("could not start the session (is the ccdash collector running with sessions allowed?): %w", err)
	}
	// The row shows up (and gets its origin) on the collector's next
	// discovery pass; wait a little so the caller gets a ref to follow.
	deadline := s.now().Add(s.startWait)
	for {
		if ss, err := s.st.ListSessions(ctx, false); err == nil {
			for _, x := range ss {
				if !known[x.SessionID] && x.Origin == s.cfg.Origin && x.Num > 0 {
					return map[string]any{"started": s.view(x, true), "note": "Claude is working on it; check with read_session in a minute or two."}, nil
				}
			}
		}
		if !s.now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.tick):
		}
	}
	out := map[string]any{"started": key, "note": "Claude is starting; it shows in list_sessions (started_by_me) within a minute."}
	if ws, err := s.st.WaitingSpawns(ctx); err == nil {
		for _, w := range ws {
			if w.Key == key {
				out["note"] = "Claude stopped at a dialog before it started; the operator has to answer it in ccdash."
			}
		}
	}
	return out, nil
}

// row is a session as the tools show it.
type row struct {
	Ref       string `json:"ref"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Note      string `json:"note,omitempty"`
	Waiting   string `json:"waiting_on_operator,omitempty"`
	StartedBy string `json:"started_by,omitempty"` // "you" for the ones it started
	LastSeen  string `json:"last_activity"`
	SessionID string `json:"session_id,omitempty"` // only its own
	URL       string `json:"url,omitempty"`        // only its own
}

// view is x as a row; mine adds what only its own sessions show.
func (s *Server) view(x model.Session, mine bool) row {
	r := row{Ref: x.Ref(), Title: oneLine(x.DisplayTitle(), 80), Status: statusOf(x), Note: oneLine(x.WaitNote, 120)}
	if r.Ref == "" {
		r.Ref = x.SessionID
	}
	if r.Title == "" {
		r.Title = "(no title yet)"
	}
	if x.PendingCount > 0 || x.Attention == model.AttentionNeedsYou {
		r.Waiting = "yes"
	}
	if t := x.LastActivity; !t.IsZero() {
		r.LastSeen = t.Local().Format("01-02 15:04")
	} else if !x.LastSeen.IsZero() {
		r.LastSeen = x.LastSeen.Local().Format("01-02 15:04")
	}
	if mine {
		r.StartedBy, r.SessionID = "you", x.SessionID
		if s.cfg.HubURL != "" {
			r.URL = s.cfg.HubURL + "/s/" + x.SessionID
		}
	}
	return r
}

// statusOf is a plain-word status: working / waiting for the operator /
// finished (with the declared kind) / stopped.
func statusOf(x model.Session) string {
	switch {
	case x.PendingCount > 0 || x.Attention == model.AttentionNeedsYou && !model.WaitNeedsYou(x.Wait):
		return "waiting for the operator (a prompt on screen)"
	case x.Status == model.StatusActive:
		return "working"
	case x.Wait == model.WaitAnswer:
		return "waiting for an answer"
	case x.Wait == model.WaitAction:
		return "waiting for the operator to do something"
	case x.Wait == model.WaitBackground:
		return "waiting on a background job"
	case x.Status == model.StatusIdle:
		return "finished its turn"
	default:
		return "stopped"
	}
}

func oneLine(t string, n int) string {
	t = strings.Join(strings.Fields(t), " ")
	r := []rune(t)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return t
}

func (s *Server) listSessions(ctx context.Context, mineOnly bool, limit int) (any, error) {
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 50)
	ss, err := s.st.ListSessions(ctx, false)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ss, func(i, j int) bool { return activity(ss[i]).After(activity(ss[j])) })
	rows := []row{}
	for _, x := range ss {
		mine := x.Origin == s.cfg.Origin
		if mineOnly && !mine {
			continue
		}
		rows = append(rows, s.view(x, mine))
		if len(rows) == limit {
			break
		}
	}
	return map[string]any{"sessions": rows}, nil
}

func activity(x model.Session) time.Time {
	if x.LastActivity.After(x.LastSeen) {
		return x.LastActivity
	}
	return x.LastSeen
}

// own finds a session it started by ref ("B231", "#231", "231") or id;
// another session is refused without saying more than that it is not its own.
func (s *Server) own(ctx context.Context, ref string) (model.Session, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return model.Session{}, fmt.Errorf("session is required")
	}
	for _, archived := range []bool{false, true} {
		ss, err := s.st.ListSessions(ctx, archived)
		if err != nil {
			return model.Session{}, err
		}
		for _, x := range ss {
			if !refMatches(x, ref) {
				continue
			}
			if x.Origin != s.cfg.Origin {
				return model.Session{}, fmt.Errorf("%s is not a session you started; only its title and status are visible (list_sessions)", ref)
			}
			return x, nil
		}
	}
	return model.Session{}, fmt.Errorf("no session %q (list_sessions with started_by_me shows yours)", ref)
}

func refMatches(x model.Session, ref string) bool {
	if ref == x.SessionID {
		return true
	}
	if x.Num <= 0 {
		return false
	}
	if strings.EqualFold(ref, x.Ref()) {
		return true
	}
	n, err := strconv.ParseInt(strings.TrimLeft(strings.TrimLeft(ref, "#"), x.HostLetter), 10, 64)
	return err == nil && n == x.Num
}

type message struct {
	At   string `json:"at,omitempty"`
	From string `json:"from"` // "request" (to Claude) / "claude"
	Text string `json:"text"`
}

func (s *Server) readSession(ctx context.Context, ref string, n int) (any, error) {
	x, err := s.own(ctx, ref)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		n = 6
	}
	n = min(n, 30)
	out := map[string]any{"session": s.view(x, true)}
	if x.TranscriptPath == "" {
		out["messages"] = []message{}
		return out, nil
	}
	tail, err := s.st.TranscriptTail(ctx, x, 512<<10)
	if err != nil {
		return nil, fmt.Errorf("read the conversation: %w", err)
	}
	var ms []message
	for _, m := range tail.Messages {
		var from string
		switch m.Kind {
		case transcript.KindUser:
			from = "request"
		case transcript.KindAssistant:
			from = "claude"
		default:
			continue // tool calls, results and thinking stay on the machine
		}
		t := strings.TrimSpace(m.Text)
		if t == "" {
			continue
		}
		e := message{From: from, Text: clip(t, 3000)}
		if !m.Timestamp.IsZero() {
			e.At = m.Timestamp.Local().Format("01-02 15:04")
		}
		ms = append(ms, e)
	}
	if len(ms) > n {
		ms = ms[len(ms)-n:]
	}
	out["messages"] = ms
	return out, nil
}

func clip(t string, n int) string {
	r := []rune(t)
	if len(r) <= n {
		return t
	}
	return string(r[:n]) + "…"
}

func (s *Server) sendMessage(ctx context.Context, ref, text string) (any, error) {
	t, err := checkText(text)
	if err != nil {
		return nil, err
	}
	x, err := s.own(ctx, ref)
	if err != nil {
		return nil, err
	}
	name := x.Ref()
	// A permission prompt or a menu would take the Enter as its answer.
	if x.PendingCount > 0 || x.Attention == model.AttentionNeedsYou && !model.WaitNeedsYou(x.Wait) {
		return nil, fmt.Errorf("%s shows a prompt for the operator right now; the operator answers it in ccdash. Try again after that", name)
	}
	hosted, err := s.st.HostedSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("ask the collector which sessions run: %w", err)
	}
	for _, h := range hosted {
		if h == x.SessionID {
			if err := s.st.SendInput(ctx, x.SessionID, store.Input{Text: t, Submit: true, Paste: strings.Contains(t, "\n")}); err != nil {
				return nil, fmt.Errorf("send to %s: %w", name, err)
			}
			return map[string]any{"sent": name, "note": "delivered; if Claude was mid-turn it reads it next. Check with read_session."}, nil
		}
	}
	if x.Status == model.StatusActive || x.Status == model.StatusIdle {
		return nil, fmt.Errorf("%s runs outside ccdash's control; the operator has to continue it", name)
	}
	if x.Cwd == "" || filepath.Clean(x.Cwd) != filepath.Clean(s.cfg.Cwd) {
		return nil, fmt.Errorf("%s did not start in the inbox directory; not resuming it", name)
	}
	if x.Archived {
		if err := s.st.SetArchived(ctx, x.SessionID, false); err != nil {
			return nil, err
		}
	}
	if _, err := s.st.StartSession(ctx, store.StartRequest{
		SessionID: x.SessionID, ResumeID: x.SessionID, Cwd: x.Cwd, Prompt: t, PermissionMode: "auto",
	}); err != nil {
		return nil, fmt.Errorf("resume %s: %w", name, err)
	}
	return map[string]any{"resumed": name, "note": "the session had stopped; it is resumed with your message. Check with read_session."}, nil
}
