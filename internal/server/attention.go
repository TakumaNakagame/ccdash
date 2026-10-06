package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/screenprompt"
)

// Attention: what a session wants from the operator, kept on the session
// row (model.Session.Attention) so the TUI, the hub's board and its push
// notifications all read the same thing.
//
//   - needs_you — a question or plan approval (PreToolUse of AskUserQuestion
//     / ExitPlanMode, cleared by the matching PostToolUse), a permission
//     prompt (Notification hook; pending approvals also count, computed in
//     db), or a dialog on a ccdash-hosted claude's screen (attentionLoop).
//   - done — the turn finished (Stop hook) and nobody has looked yet; the
//     TUI selecting the session or the portal opening it marks it seen.
//   - a new prompt clears whatever was there.

// attentionTools are tool calls that wait on the operator.
var attentionTools = map[string]string{
	"AskUserQuestion": "質問",
	"ExitPlanMode":    "プランの承認",
}

func (s *Server) attentionFromPreTool(ctx context.Context, p *hookPayload) {
	label, ok := attentionTools[p.ToolName]
	if !ok {
		return
	}
	reason := label
	if p.ToolName == "AskUserQuestion" {
		var in struct {
			Questions []struct {
				Question string `json:"question"`
			} `json:"questions"`
		}
		if json.Unmarshal(p.ToolInput, &in) == nil && len(in.Questions) > 0 && in.Questions[0].Question != "" {
			reason = label + ": " + truncate(in.Questions[0].Question, 120)
		}
	}
	if err := s.db.SetAttention(ctx, p.SessionID, model.AttentionNeedsYou, reason); err != nil {
		log.Printf("attention: %v", err)
	}
}

func (s *Server) attentionFromPostTool(ctx context.Context, p *hookPayload) {
	if _, ok := attentionTools[p.ToolName]; ok {
		_ = s.db.ClearAttention(ctx, p.SessionID, model.AttentionNeedsYou)
	}
}

// attentionFromNotification marks permission prompts. Claude Code's other
// notification ("waiting for your input" after a minute idle) is just a
// finished turn, which Stop already covers.
func (s *Server) attentionFromNotification(ctx context.Context, p *hookPayload) {
	msg := strings.TrimSpace(p.Message)
	if msg == "" || !strings.Contains(strings.ToLower(msg), "permission") {
		return
	}
	_ = s.db.SetAttention(ctx, p.SessionID, model.AttentionNeedsYou, truncate(msg, 120))
}

// attentionLoop watches the screens of ccdash-hosted claudes for dialogs
// that have no hook (a permission prompt with approvals off, the folder
// trust question, a menu) and mirrors them into the session's attention.
func (s *Server) attentionLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	shown := map[string]string{} // session id → reason we set from the screen
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.ptyMu.Lock()
		entries := map[string]*ptyEntry{}
		for k, e := range s.ptyMap {
			if !strings.HasPrefix(k, "pid-") && !e.exited.Load() {
				entries[k] = e
			}
		}
		s.ptyMu.Unlock()
		for sid, e := range entries {
			rows, _, _, _ := e.snapshot()
			for i, r := range rows {
				rows[i] = strings.TrimRight(ansi.Strip(r), " ")
			}
			kind, text := screenprompt.Kind(rows)
			switch {
			case kind != "":
				reason := "確認"
				if kind == "question" {
					reason = "質問"
				}
				if text != "" {
					reason += ": " + truncate(text, 120)
				}
				if shown[sid] != reason {
					shown[sid] = reason
					_ = s.db.SetAttention(ctx, sid, model.AttentionNeedsYou, reason)
				}
			case shown[sid] != "":
				delete(shown, sid)
				_ = s.db.ClearAttention(ctx, sid, model.AttentionNeedsYou)
			}
		}
		for sid := range shown {
			if _, ok := entries[sid]; !ok {
				delete(shown, sid)
			}
		}
	}
}

// handleAPISeen marks a session as looked at (an unread "done" becomes read).
// POST /api/sessions/{id}/seen
func (s *Server) handleAPISeen(w http.ResponseWriter, r *http.Request) {
	if err := s.db.MarkSeen(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, nil)
}
