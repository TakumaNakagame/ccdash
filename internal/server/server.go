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
	"time"

	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/discovery"
	"github.com/takumanakagame/ccmanage/internal/gitinfo"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/procmap"
)

type Server struct {
	db   *db.DB
	mux  *http.ServeMux
	addr string
	srv  *http.Server
}

func New(d *db.DB, addr string) *Server {
	s := &Server{db: d, addr: addr, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	s.mux.HandleFunc("/hooks/session-start", s.handleSessionStart)
	s.mux.HandleFunc("/hooks/session-end", s.handleSessionEnd)
	s.mux.HandleFunc("/hooks/user-prompt", s.handleUserPrompt)
	s.mux.HandleFunc("/hooks/pre-tool", s.handlePreTool)
	s.mux.HandleFunc("/hooks/post-tool", s.handlePostTool)
	s.mux.HandleFunc("/hooks/post-tool-failure", s.handlePostToolFailure)
	s.mux.HandleFunc("/hooks/permission-request", s.handlePermissionRequest)
	s.mux.HandleFunc("/hooks/stop", s.handleStop)
	s.mux.HandleFunc("/hooks/subagent-stop", s.handleSubagentStop)
	s.mux.HandleFunc("/hooks/notification", s.handleNotification)
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
	go s.discoveryLoop(ctx)
	if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
	return &p, json.RawMessage(body), nil
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
	_ = s.db.ResolvePendingByToolUseID(r.Context(), p.SessionID, p.ToolUseID, model.ApprovalResolved)
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
	writeOK(w, nil)
}

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
	_, _ = s.db.InsertApproval(r.Context(), &model.Approval{
		SessionID: p.SessionID,
		Tool:      p.ToolName,
		ToolUseID: p.ToolUseID,
		ToolInput: p.ToolInput,
		Status:    model.ApprovalPending,
	})
	// Phase 1: observation only — return empty so Claude falls back to its
	// normal interactive permission flow.
	writeOK(w, nil)
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
