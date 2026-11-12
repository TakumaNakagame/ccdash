package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/TakumaNakagame/ccdash/internal/claudecfg"
	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/settings"
)

// Automated claude runs — a cron job's `claude -p`, `claude mcp list` —
// never show in ccdash. A run is automated when
//
//   - its environment has CCDASH_IGNORE set (hook.sh / the PermissionRequest
//     hook forward it as X-Ccdash-Ignore), or
//   - it runs headless (claude -p: an sdk-* entrypoint) with its cwd under
//     the ignore_dirs setting,
//
// unless ccdash started it (a hosted PTY's hook tag), so a session the
// operator starts with n / the portal / MCP start_session always shows.
// The first such hook records the session ID (db.IgnoreSession) and every
// later hook for it, and discovery's transcript scan, skip it.
//
// Separately, a SessionEnd for a session ccdash never saw creates nothing
// (handleSessionEnd): subcommands like `claude mcp list` fire only that.

// ignoreHeader carries $CCDASH_IGNORE.
const ignoreHeader = "X-Ccdash-Ignore"

// ignoreFlag reports whether a CCDASH_IGNORE value asks to be ignored.
func ignoreFlag(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v != "" && v != "0" && v != "false" && v != "no"
}

// ignoreGate wraps a hook handler: an automated run's event is answered
// with {} (for PermissionRequest: no decision, Claude carries on as
// without ccdash) and nothing is stored.
func (s *Server) ignoreGate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		_ = r.Body.Close()
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var p struct {
			SessionID string `json:"session_id"`
			Cwd       string `json:"cwd"`
		}
		_ = json.Unmarshal(body, &p)
		if p.SessionID != "" && s.ignoreHook(r, p.SessionID, p.Cwd) {
			writeOK(w, nil)
			return
		}
		h(w, r)
	}
}

// ignoreHook decides (and records) whether this hook belongs to an
// automated run.
func (s *Server) ignoreHook(r *http.Request, id, cwd string) bool {
	ctx := r.Context()
	tag, _ := strconv.Atoi(headerVal(r, "X-Ccdash-Wrapper-Pid"))
	if tag >= ptyTagBase {
		return false // ccdash started it
	}
	if s.db.IsIgnored(ctx, id) {
		return true
	}
	dirRule := model.Headless(headerVal(r, "X-Ccdash-Entrypoint")) && s.ignoredDir(ctx, cwd)
	if !ignoreFlag(headerVal(r, ignoreHeader)) && !dirRule {
		return false
	}
	ok, err := s.db.IgnoreSession(ctx, id)
	if err != nil {
		log.Printf("ignore session %s: %v", id, err)
		return false
	}
	return ok // false: the operator kept this row; treat it normally
}

// ignoredDir reports whether cwd is under the ignore_dirs setting.
func (s *Server) ignoredDir(ctx context.Context, cwd string) bool {
	if cwd == "" {
		return false
	}
	cfg, _ := settings.Load(ctx, s.db)
	return cfg.IgnoreDirs != "" && claudecfg.Under(cwd, cfg.IgnoreDirs)
}

// discoverySkip is the transcript scan's side of the same rule: a session
// recorded as ignored, or a new headless one under ignore_dirs (the
// collector was down while it ran, so no hook told us), is never upserted.
// A row that already exists or a session ccdash hosts is kept.
func (s *Server) discoverySkip(ctx context.Context, ignored map[string]bool, ignoreDirs, id, cwd, entrypoint string) bool {
	if ignored[id] {
		return true
	}
	if ignoreDirs == "" || !model.Headless(entrypoint) || !claudecfg.Under(cwd, ignoreDirs) {
		return false
	}
	if s.lookupPTY(id) != nil || s.db.SessionExists(ctx, id) {
		return false
	}
	if _, err := s.db.IgnoreSession(ctx, id); err != nil {
		log.Printf("ignore session %s: %v", id, err)
	}
	ignored[id] = true
	return true
}
