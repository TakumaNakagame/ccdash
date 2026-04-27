package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/takumanakagame/ccmanage/internal/auth"
	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/discovery"
	"github.com/takumanakagame/ccmanage/internal/gitinfo"
	"github.com/takumanakagame/ccmanage/internal/hookcfg"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/procmap"
	"github.com/takumanakagame/ccmanage/internal/redact"
)

type Server struct {
	db    *db.DB
	mux   *http.ServeMux
	addr  string
	srv   *http.Server
	token string // shared secret; required on every hook + decision request

	// pending tracks PermissionRequest hooks that are still blocking
	// inside the handler waiting for an operator decision. The TUI POSTs
	// to /approvals/<id>/decide which routes the message to the matching
	// channel and unblocks the handler.
	pendingMu sync.Mutex
	pending   map[int64]chan approvalDecision
}

type approvalDecision struct {
	Behavior string // "allow" or "deny"
	Reason   string
	// Keep, when true on an "allow" decision, asks Claude to remember the
	// permission for the rest of the session via hookSpecificOutput
	// updatedPermissions. We synthesize a rule from the tool name (and the
	// first token of a Bash command) so the next equivalent call doesn't
	// prompt again.
	Keep bool
}

func New(d *db.DB, addr string) *Server {
	tok, err := auth.LoadOrCreate()
	if err != nil {
		// Fall back to an empty token, which causes all auth-checked routes
		// to refuse traffic with 503. The operator's logs will show the
		// underlying error from auth.LoadOrCreate.
		log.Printf("auth token: %v", err)
	}
	s := &Server{
		db:      d,
		addr:    addr,
		mux:     http.NewServeMux(),
		token:   tok,
		pending: map[int64]chan approvalDecision{},
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	// /healthz stays unauthenticated so the TUI can detect a running
	// server before deciding whether to spawn an embedded one. It returns
	// only "ok"; no sensitive info leaks.
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	s.mux.HandleFunc("/hooks/session-start", s.requireToken(s.handleSessionStart))
	s.mux.HandleFunc("/hooks/session-end", s.requireToken(s.handleSessionEnd))
	s.mux.HandleFunc("/hooks/user-prompt", s.requireToken(s.handleUserPrompt))
	s.mux.HandleFunc("/hooks/pre-tool", s.requireToken(s.handlePreTool))
	s.mux.HandleFunc("/hooks/post-tool", s.requireToken(s.handlePostTool))
	s.mux.HandleFunc("/hooks/post-tool-failure", s.requireToken(s.handlePostToolFailure))
	s.mux.HandleFunc("/hooks/permission-request", s.requireToken(s.handlePermissionRequest))
	s.mux.HandleFunc("/hooks/stop", s.requireToken(s.handleStop))
	s.mux.HandleFunc("/hooks/subagent-stop", s.requireToken(s.handleSubagentStop))
	s.mux.HandleFunc("/hooks/notification", s.requireToken(s.handleNotification))
	s.mux.HandleFunc("/approvals/", s.requireToken(s.handleApprovalDecide))
}

// requireToken wraps a handler so requests without a matching X-Ccdash-Token
// header are refused before reaching any side-effecting code. If the server
// failed to load a token at startup we refuse everything with 503 — better
// to be inert than silently unauthenticated.
func (s *Server) requireToken(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			http.Error(w, "ccdash: no auth token configured", http.StatusServiceUnavailable)
			return
		}
		got := r.Header.Get(auth.HeaderName)
		if got == "" || got != s.token {
			http.Error(w, "ccdash: bad or missing token", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.srv = &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("ccdash server listening on http://%s", ln.Addr())
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()
	s.syncInstalledHooks()
	go s.discoveryLoop(ctx)
	if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// syncInstalledHooks compares the X-Ccdash-Token currently baked into
// ~/.claude/settings.json with the live one we loaded at startup. When they
// disagree (rotation, fresh state dir, install-hooks never ran, etc.) we
// rewrite the hook entries automatically so the operator doesn't have to
// remember to re-run install-hooks.
//
// Skipped silently when no hook entries are present yet — a brand new
// install where the user hasn't run install-hooks at all should stay
// hands-off.
func (s *Server) syncInstalledHooks() {
	if s.token == "" {
		return
	}
	in, err := hookcfg.DefaultInstall()
	if err != nil {
		log.Printf("hooks sync: %v", err)
		return
	}
	have, err := hookcfg.InstalledTokenAt(in.Path)
	if err != nil {
		log.Printf("hooks sync read: %v", err)
		return
	}
	if have == "" {
		// Operator hasn't run install-hooks. Don't ambush them by writing
		// to settings.json on first server start — they may be evaluating
		// ccdash without wanting hooks installed yet.
		return
	}
	if have == s.token {
		return
	}
	if _, err := in.Apply(); err != nil {
		log.Printf("hooks auto-resync: %v", err)
		return
	}
	log.Printf("hooks: rewrote %s with current token", in.Path)
}

// discoveryLoop runs an initial transcript scan and then refreshes every 10s.
// This populates and updates the sessions table from on-disk transcripts and
// running claude processes, so idle sessions appear without needing to wait
// for a hook event.
func (s *Server) discoveryLoop(ctx context.Context) {
	if err := s.refreshDiscovery(ctx); err != nil {
		log.Printf("discovery: %v", err)
	}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.refreshDiscovery(ctx); err != nil {
				log.Printf("discovery: %v", err)
			}
		}
	}
}

func (s *Server) refreshDiscovery(ctx context.Context) error {
	// Sweep stale approvals first — anything pending for longer than Claude's
	// own 30-second hook timeout cannot still be waiting on us.
	if err := s.db.MarkStalePendingTimeout(ctx, 45*time.Second); err != nil {
		log.Printf("approval sweep: %v", err)
	}
	discovered, err := discovery.Scan(ctx, "")
	if err != nil {
		return err
	}
	procs, _ := procmap.Snapshot(ctx)
	now := time.Now()

	for _, d := range discovered {
		sess := &model.Session{
			SessionID:      d.SessionID,
			Cwd:            d.Cwd,
			Branch:         d.GitBranch,
			Title:          d.Title,
			TranscriptPath: d.TranscriptPath,
			LastSeen:       d.LastModified,
			Status:         classifyStatus(d.LastModified, now, ""),
		}
		if entry, ok := procs[d.SessionID]; ok {
			sess.ProcPID = entry.PID
			sess.Pane = entry.Pane
			if entry.TmuxSession != "" {
				sess.TmuxSession = entry.TmuxSession
			}
			sess.Status = classifyStatus(d.LastModified, now, entry.ClaudeStatus)
		}
		// Derive git info from cwd when discovery's gitBranch was unhelpful
		// (transcripts often record "HEAD" rather than the actual branch).
		if (sess.Branch == "" || sess.Branch == "HEAD") && sess.Cwd != "" {
			g := gitinfo.Lookup(ctx, sess.Cwd)
			if g.Branch != "" {
				sess.Branch = g.Branch
			}
			if sess.Repo == "" {
				sess.Repo = g.Repo
			}
			if sess.Commit == "" {
				sess.Commit = g.Commit
			}
		}
		if err := s.db.UpsertSession(ctx, sess); err != nil {
			log.Printf("discovery upsert %s: %v", d.SessionID, err)
		}
	}
	return nil
}

// classifyStatus maps Claude's authoritative session state ("busy" / "idle")
// to ccdash's status taxonomy. When Claude is not running for the session
// (claudeStatus is empty), we infer from the transcript mtime: anything
// touched within the last 6 hours stays in the "recent" bucket so it shows
// up as something the operator probably still cares about.
func classifyStatus(mtime, now time.Time, claudeStatus string) model.SessionStatus {
	switch claudeStatus {
	case "busy":
		return model.StatusActive
	case "idle":
		return model.StatusIdle
	}
	if now.Sub(mtime) < 6*time.Hour {
		return model.StatusRecent
	}
	return model.StatusStopped
}

// hookPayload covers the union of all hook inputs we care about.
// Unknown fields are preserved in `raw` for storage.
type hookPayload struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	PermissionMode string          `json:"permission_mode"`
	Source         string          `json:"source"`
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	ToolUseID      string          `json:"tool_use_id"`
	Error          string          `json:"error"`
	DurationMS     int64           `json:"duration_ms"`
}

func readPayload(r *http.Request) (*hookPayload, json.RawMessage, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		return nil, nil, err
	}
	defer r.Body.Close()
	if len(body) == 0 {
		return &hookPayload{}, json.RawMessage("{}"), nil
	}
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, nil, fmt.Errorf("decode payload: %w", err)
	}
	// Mask common secret patterns before anything reaches the DB.
	masked := redact.JSON(body)
	p.Prompt = redact.String(p.Prompt)
	p.Error = redact.String(p.Error)
	if len(p.ToolInput) > 0 {
		p.ToolInput = redact.JSON(p.ToolInput)
	}
	if len(p.ToolResponse) > 0 {
		p.ToolResponse = redact.JSON(p.ToolResponse)
	}
	return &p, json.RawMessage(masked), nil
}

func writeOK(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	if body == nil {
		_, _ = w.Write([]byte("{}"))
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	log.Printf("hook error: %v", err)
	http.Error(w, err.Error(), status)
}

func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	p, raw, err := readPayload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.ensureSession(r, p, model.StatusActive); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := s.db.AppendEvent(r.Context(), &model.Event{
		SessionID: p.SessionID,
		EventType: model.EventSessionStart,
		Summary:   "session started (" + p.Source + ")",
		Payload:   raw,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, nil)
}

// ensureSession creates or updates the session row using whatever metadata is
// available on this hook event. We call it from every handler so that
// already-running Claude sessions (which started before `ccdash install-hooks`
// and therefore never emit a SessionStart event) still appear in the dashboard
// once they fire any other hook.
func (s *Server) ensureSession(r *http.Request, p *hookPayload, status model.SessionStatus) error {
	if p.SessionID == "" {
		return nil
	}
	pid, _ := strconv.Atoi(headerVal(r, "X-Ccdash-Wrapper-Pid"))
	sess := &model.Session{
		SessionID:      p.SessionID,
		Cwd:            p.Cwd,
		Repo:           headerVal(r, "X-Ccdash-Git-Repo"),
		Branch:         headerVal(r, "X-Ccdash-Git-Branch"),
		Commit:         headerVal(r, "X-Ccdash-Git-Commit"),
		WrapperPID:     pid,
		TmuxPane:       headerVal(r, "X-Ccdash-Tmux-Pane"),
		TmuxSession:    headerVal(r, "X-Ccdash-Tmux-Session"),
		TranscriptPath: p.TranscriptPath,
		Model:          p.Model,
		Status:         status,
	}
	if sess.Repo == "" && sess.Branch == "" && sess.Commit == "" && sess.Cwd != "" {
		g := gitinfo.Lookup(r.Context(), sess.Cwd)
		sess.Repo = g.Repo
		sess.Branch = g.Branch
		sess.Commit = g.Commit
	}
	return s.db.UpsertSession(r.Context(), sess)
}

func (s *Server) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	p, raw, err := readPayload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.ensureSession(r, p, model.StatusStopped)
	_, _ = s.db.AppendEvent(r.Context(), &model.Event{
		SessionID: p.SessionID,
		EventType: model.EventSessionEnd,
		Summary:   "session ended",
		Payload:   raw,
	})
	writeOK(w, nil)
}

func (s *Server) handleUserPrompt(w http.ResponseWriter, r *http.Request) {
	p, raw, err := readPayload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.ensureSession(r, p, model.StatusActive)
	summary := truncate(strings.TrimSpace(p.Prompt), 200)
	_, _ = s.db.AppendEvent(r.Context(), &model.Event{
		SessionID: p.SessionID,
		EventType: model.EventUserPrompt,
		Summary:   summary,
		Payload:   raw,
	})
	writeOK(w, nil)
}

func (s *Server) handlePreTool(w http.ResponseWriter, r *http.Request) {
	p, raw, err := readPayload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.ensureSession(r, p, model.StatusActive)
	_, _ = s.db.AppendEvent(r.Context(), &model.Event{
		SessionID: p.SessionID,
		EventType: model.EventPreTool,
		Tool:      p.ToolName,
		Summary:   summarizeToolInput(p.ToolName, p.ToolInput),
		Payload:   raw,
	})
	writeOK(w, nil)
}

func (s *Server) handlePostTool(w http.ResponseWriter, r *http.Request) {
	p, raw, err := readPayload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.ensureSession(r, p, model.StatusActive)
	summary := summarizeToolInput(p.ToolName, p.ToolInput)
	if p.DurationMS > 0 {
		summary = fmt.Sprintf("%s (%dms)", summary, p.DurationMS)
	}
	_, _ = s.db.AppendEvent(r.Context(), &model.Event{
		SessionID: p.SessionID,
		EventType: model.EventPostTool,
		Tool:      p.ToolName,
		Summary:   summary,
		Payload:   raw,
	})
	if err := s.db.ResolvePendingByToolUseID(r.Context(), p.SessionID, p.ToolUseID, model.ApprovalResolved); err != nil {
		log.Printf("resolve approval: %v", err)
	}
	// PermissionRequest hooks don't carry tool_use_id, so the call above only
	// matches when we got lucky. Fall back to closing the oldest pending
	// approval with the same session+tool — that's almost always the one.
	_ = s.db.ResolveOldestPendingForTool(r.Context(), p.SessionID, p.ToolName, model.ApprovalResolved)
	writeOK(w, nil)
}

func (s *Server) handlePostToolFailure(w http.ResponseWriter, r *http.Request) {
	p, raw, err := readPayload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.ensureSession(r, p, model.StatusActive)
	summary := p.ToolName
	if p.Error != "" {
		summary = fmt.Sprintf("%s: %s", p.ToolName, truncate(p.Error, 200))
	}
	_, _ = s.db.AppendEvent(r.Context(), &model.Event{
		SessionID: p.SessionID,
		EventType: model.EventPostToolFailure,
		Tool:      p.ToolName,
		Summary:   summary,
		Payload:   raw,
	})
	_ = s.db.ResolvePendingByToolUseID(r.Context(), p.SessionID, p.ToolUseID, model.ApprovalFailed)
	_ = s.db.ResolveOldestPendingForTool(r.Context(), p.SessionID, p.ToolName, model.ApprovalFailed)
	writeOK(w, nil)
}

// handlePermissionRequest holds the hook response open for up to 25s waiting
// for a TUI operator decision. If one arrives we return the appropriate
// hookSpecificOutput JSON so Claude obeys (allow/deny without prompting). If
// nothing arrives we let the request fall through with an empty body so
// Claude shows its own interactive prompt — exactly the pre-Phase-2
// behavior.
func (s *Server) handlePermissionRequest(w http.ResponseWriter, r *http.Request) {
	p, raw, err := readPayload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.ensureSession(r, p, model.StatusActive)
	_, _ = s.db.AppendEvent(r.Context(), &model.Event{
		SessionID: p.SessionID,
		EventType: model.EventPermissionRequest,
		Tool:      p.ToolName,
		Summary:   summarizeToolInput(p.ToolName, p.ToolInput),
		Payload:   raw,
	})
	id, err := s.db.InsertApproval(r.Context(), &model.Approval{
		SessionID: p.SessionID,
		Tool:      p.ToolName,
		ToolUseID: p.ToolUseID,
		ToolInput: p.ToolInput,
		Status:    model.ApprovalPending,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	ch := make(chan approvalDecision, 1)
	s.pendingMu.Lock()
	s.pending[id] = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
	}()

	const decideWindow = 25 * time.Second // Claude's hook timeout is 30s; leave headroom.
	select {
	case d := <-ch:
		status := model.ApprovalApproved
		if d.Behavior == "deny" {
			status = model.ApprovalDenied
		}
		_ = s.db.UpdateApprovalStatus(r.Context(), id, status, d.Reason)
		decision := map[string]any{
			"behavior": d.Behavior,
			"message":  d.Reason,
		}
		if d.Keep && d.Behavior == "allow" {
			decision["updatedPermissions"] = []map[string]any{{
				"rule":  ruleFor(p.ToolName, p.ToolInput),
				"scope": "session",
			}}
		}
		writeOK(w, map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName": "PermissionRequest",
				"decision":      decision,
			},
		})
	case <-time.After(decideWindow):
		_ = s.db.UpdateApprovalStatus(r.Context(), id, model.ApprovalTimeout, "ccdash: no decision within 25s")
		writeOK(w, nil)
	case <-r.Context().Done():
		// Claude gave up. Leave status as pending; the discovery loop will
		// timeout-sweep it.
	}
}

// handleApprovalDecide accepts POST /approvals/<id>/decide with body
// {"behavior":"allow|deny","reason":"..."} and routes it to the matching
// PermissionRequest goroutine. Returns 410 if no handler is waiting (the
// approval was already resolved or timed out).
func (s *Server) handleApprovalDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/approvals/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[1] != "decide" {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Behavior string `json:"behavior"`
		Reason   string `json:"reason"`
		Keep     bool   `json:"keep"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Behavior != "allow" && body.Behavior != "deny" {
		http.Error(w, "behavior must be 'allow' or 'deny'", http.StatusBadRequest)
		return
	}
	s.pendingMu.Lock()
	ch, ok := s.pending[id]
	s.pendingMu.Unlock()
	if !ok {
		http.Error(w, "no pending hold for that id (already resolved or timed out)", http.StatusGone)
		return
	}
	select {
	case ch <- approvalDecision{Behavior: body.Behavior, Reason: body.Reason, Keep: body.Keep}:
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		http.Error(w, "decision already accepted", http.StatusConflict)
	}
}

// ruleFor builds a Claude permission rule for the given tool / input. For
// Bash we glob on the first whitespace-separated token of the command so
// subsequent variants of the same command (`git status` → `git diff` →
// `git log`) don't re-prompt; for everything else we allow the whole tool
// kind, which is what matches Claude's own "always allow this tool"
// shortcut.
func ruleFor(tool string, input json.RawMessage) string {
	if tool == "Bash" && len(input) > 0 {
		var m struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(input, &m); err == nil {
			cmd := strings.TrimSpace(m.Command)
			if cmd != "" {
				if i := strings.IndexAny(cmd, " \t"); i > 0 {
					return fmt.Sprintf("Bash(%s *)", cmd[:i])
				}
				return fmt.Sprintf("Bash(%s)", cmd)
			}
		}
		return "Bash"
	}
	return tool
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	p, raw, err := readPayload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.ensureSession(r, p, model.StatusIdle)
	_, _ = s.db.AppendEvent(r.Context(), &model.Event{
		SessionID: p.SessionID,
		EventType: model.EventStop,
		Summary:   "stop",
		Payload:   raw,
	})
	writeOK(w, nil)
}

func (s *Server) handleSubagentStop(w http.ResponseWriter, r *http.Request) {
	p, raw, err := readPayload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	_, _ = s.db.AppendEvent(r.Context(), &model.Event{
		SessionID: p.SessionID,
		EventType: model.EventSubagentStop,
		Summary:   "subagent stop",
		Payload:   raw,
	})
	writeOK(w, nil)
}

func (s *Server) handleNotification(w http.ResponseWriter, r *http.Request) {
	p, raw, err := readPayload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	_, _ = s.db.AppendEvent(r.Context(), &model.Event{
		SessionID: p.SessionID,
		EventType: model.EventNotification,
		Summary:   "notification",
		Payload:   raw,
	})
	writeOK(w, nil)
}

// headerVal returns the header value or "" if it's empty or an unfilled
// `${VAR}` interpolation placeholder (which can occur when the corresponding
// env var was not set at the time Claude Code emitted the hook).
func headerVal(r *http.Request, key string) string {
	v := strings.TrimSpace(r.Header.Get(key))
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
		return ""
	}
	return v
}

func summarizeToolInput(tool string, input json.RawMessage) string {
	if len(input) == 0 {
		return tool
	}
	// Defensive: redact again before computing the human-readable summary
	// in case the caller skipped the payload-level pass.
	input = redact.JSON(input)
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return tool
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if str, ok := v.(string); ok && str != "" {
					return str
				}
			}
		}
		return ""
	}
	switch tool {
	case "Bash":
		return truncate(pick("command"), 200)
	case "Edit", "Write", "Read":
		return pick("file_path")
	case "Glob", "Grep":
		return pick("pattern", "query")
	case "WebFetch", "WebSearch":
		return pick("url", "query")
	}
	if s := pick("file_path", "command", "url", "query", "pattern"); s != "" {
		return truncate(s, 200)
	}
	return tool
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
