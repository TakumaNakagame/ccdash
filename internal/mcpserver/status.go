package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// setStatus records what the calling session hands back to the operator
// (the status rule in instructions). The collector turns answer / action
// into "needs you" when the turn's Stop arrives.
func (s *Server) setStatus(ctx context.Context, status, note, session string) (any, error) {
	if model.WaitLabel(status) == "" {
		return nil, fmt.Errorf("status must be one of %s", strings.Join(model.WaitKinds, ", "))
	}
	sid := strings.TrimSpace(session)
	if sid == "" {
		sid = s.self()
	}
	if sid == "" {
		return nil, fmt.Errorf("can't tell which session is calling; pass session = your session id")
	}
	x, err := s.findSession(ctx, sid)
	if err != nil {
		// The first hook of a brand-new session may not have landed yet.
		return nil, fmt.Errorf("ccdash doesn't know session %s yet: %w", sid, err)
	}
	note = clip(strings.Join(strings.Fields(note), " "), 200)
	if err := s.st.SetWait(ctx, x.SessionID, status, note); err != nil {
		return nil, err
	}
	return map[string]any{"session": refsOf([]model.Session{x})[0], "status": status, "shown_as": model.WaitLabel(status), "note": note}, nil
}

// selfSession finds the claude session this server runs under. Claude Code
// starts MCP servers as its children and keeps ~/.claude/sessions/<pid>.json
// current (it follows /clear and --resume), so walk up the parents looking
// for one; else trust $CLAUDE_CODE_SESSION_ID from the environment.
func selfSession() string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	pid := os.Getppid()
	for range 4 {
		if pid <= 1 {
			break
		}
		if b, err := os.ReadFile(filepath.Join(dir, "sessions", strconv.Itoa(pid)+".json")); err == nil {
			var v struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(b, &v) == nil && v.SessionID != "" {
				return v.SessionID
			}
		}
		pid = parentOf(pid)
	}
	return os.Getenv("CLAUDE_CODE_SESSION_ID")
}

// parentOf reads a process's parent from /proc (Linux); 0 elsewhere, where
// the direct parent — the usual case — has already been tried.
func parentOf(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// "pid (comm) state ppid …" — comm may hold spaces and parens.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}
