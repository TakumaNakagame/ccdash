package server

// Restarting things without losing the hosted claude sessions:
//
//   - POST /pty/{key}/restart kills one hosted claude and starts
//     `claude --resume <session>` in its place (same key, cwd and size).
//   - POST /api/restart saves the hosted sessions to resume-on-start.json and
//     stops the server with ErrRestart, on which `ccdash server` or
//     `ccdash web` (the ones that set CanRestart) re-execs itself — the
//     TUI's "Restart ccdash" uses it. `ccdash web stop --resume`, and the
//     TUI against a collector without /api/restart, do the same save via
//     POST /shutdown?resume=1, which stops the collector for good (and with
//     it a `ccdash web` that runs it) — the next start resumes.
//   - Whichever way the server stops, its hosted claudes are closed
//     (ListenAndServe), so a resumed claude never runs next to the old one.
//   - On start, resumeSaved brings back what a recent save lists. The file
//     is consumed once and ignored when stale, so a reboot days later does
//     not suddenly launch claude everywhere.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/paths"
	"github.com/takumanakagame/ccmanage/internal/tracing"
)

// ErrRestart is what ListenAndServe returns after POST /api/restart: the
// caller re-execs the collector.
var ErrRestart = errors.New("ccdash: collector restart requested")

// resumeMaxAge bounds how old a resume-on-start.json may be to be honored.
const resumeMaxAge = 10 * time.Minute

// resumeEntry is one hosted session to bring back after a restart.
type resumeEntry struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

func resumeFilePath() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "resume-on-start.json"), nil
}

// sessionKeyOf returns the session-ID key of the PTY known as key (key
// itself unless it is a pid-<N> key), or "" while it has no alias yet.
// Caller holds ptyMu.
func (s *Server) sessionKeyOf(key string) (string, *ptyEntry) {
	e, ok := s.ptyMap[key]
	if !ok {
		return "", nil
	}
	if !strings.HasPrefix(key, "pid-") {
		return key, e
	}
	for k, o := range s.ptyMap {
		if o == e && !strings.HasPrefix(k, "pid-") {
			return k, e
		}
	}
	return "", e
}

// hostedSessions lists the alive PTYs that hold a known session, with the
// cwd recorded for it (falling back to nothing: claude then starts in the
// collector's cwd, which --resume would refuse — so those are skipped).
func (s *Server) hostedSessions(ctx context.Context) []resumeEntry {
	s.ptyMu.Lock()
	type hosted struct {
		sid        string
		cols, rows int
	}
	var hs []hosted
	for k, e := range s.ptyMap {
		if strings.HasPrefix(k, "pid-") || !e.sess.Alive() {
			continue
		}
		c, rw := e.size()
		hs = append(hs, hosted{k, c, rw})
	}
	s.ptyMu.Unlock()
	var out []resumeEntry
	for _, h := range hs {
		sess, ok, err := s.db.GetSession(ctx, h.sid)
		if err != nil || !ok || sess.Cwd == "" {
			continue
		}
		out = append(out, resumeEntry{SessionID: h.sid, Cwd: sess.Cwd, Cols: h.cols, Rows: h.rows})
	}
	return out
}

// saveResumeList writes the hosted sessions for the next start to resume.
func (s *Server) saveResumeList(ctx context.Context) (int, error) {
	list := s.hostedSessions(ctx)
	p, err := resumeFilePath()
	if err != nil {
		return 0, err
	}
	b, _ := json.Marshal(list)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return 0, err
	}
	return len(list), nil
}

// resumeSaved starts `claude --resume` for each session in a fresh
// resume-on-start.json, then deletes it. Spaced out: every claude spins
// up its MCP servers at once otherwise.
func (s *Server) resumeSaved() {
	p, err := resumeFilePath()
	if err != nil {
		return
	}
	fi, err := os.Stat(p)
	if err != nil {
		return
	}
	b, err := os.ReadFile(p)
	_ = os.Remove(p)
	if err != nil || time.Since(fi.ModTime()) > resumeMaxAge {
		return
	}
	var list []resumeEntry
	if json.Unmarshal(b, &list) != nil {
		return
	}
	ctx, end := tracing.Span(context.Background(), "collector.resume_saved",
		attribute.Int("ccdash.sessions", len(list)))
	defer end(nil)
	for i, e := range list {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		if _, _, _, err := s.startPTY(ctx, ptyStartReq{SessionID: e.SessionID, ResumeID: e.SessionID, Cwd: e.Cwd, Cols: e.Cols, Rows: e.Rows}); err != nil {
			log.Printf("resume %s: %v", e.SessionID, err)
			continue
		}
		log.Printf("resumed %s after restart", e.SessionID)
	}
}

// closeAllPTYs ends every hosted claude (when the server stops).
func (s *Server) closeAllPTYs() {
	s.ptyMu.Lock()
	seen := map[*ptyEntry]bool{}
	for _, e := range s.ptyMap {
		seen[e] = true
	}
	s.ptyMap = map[string]*ptyEntry{}
	s.ptyMu.Unlock()
	for e := range seen {
		_ = e.sess.Close()
	}
}

// handlePTYRestart: kill the claude in PTY id and resume its session in a
// new PTY under the same session key. 409 while the PTY has no session yet
// (a fresh spawn before its first prompt — nothing to resume).
func (s *Server) handlePTYRestart(w http.ResponseWriter, r *http.Request, id string) {
	s.ptyMu.Lock()
	sid, entry := s.sessionKeyOf(id)
	s.ptyMu.Unlock()
	if entry == nil {
		http.Error(w, "pty not found", http.StatusNotFound)
		return
	}
	if sid == "" {
		http.Error(w, "this claude has no session yet — nothing to resume", http.StatusConflict)
		return
	}
	sess, ok, err := s.db.GetSession(r.Context(), sid)
	if err != nil || !ok || sess.Cwd == "" {
		http.Error(w, "session cwd unknown", http.StatusConflict)
		return
	}
	cols, rows := entry.size()
	s.ptyMu.Lock()
	for k, e := range s.ptyMap {
		if e == entry {
			delete(s.ptyMap, k)
		}
	}
	s.ptyMu.Unlock()
	_ = entry.sess.Close()
	select {
	case <-entry.sess.ChildExit():
	case <-time.After(5 * time.Second):
	}
	key, _, code, err := s.startPTY(r.Context(), ptyStartReq{SessionID: sid, ResumeID: sid, Cwd: sess.Cwd, Cols: cols, Rows: rows})
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	writeOK(w, map[string]string{"ptyKey": key})
}

// handleRestart: POST /api/restart — save the hosted sessions and re-exec the
// collector, which resumes them on start. With {"when":"idle"} it only
// schedules that for when no hosted claude is busy (restartwait.go). 501
// where the process can't re-exec itself (CanRestart unset).
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if !s.CanRestart || s.cancelFn == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("this collector cannot restart itself; restart it from where it runs"))
		return
	}
	var req restartReq
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("bad body: %w", err))
			return
		}
	}
	switch req.When {
	case "", model.RestartNow:
	case model.RestartWhenIdle:
		writeOK(w, s.scheduleRestart(req))
		return
	default:
		writeErr(w, http.StatusBadRequest, fmt.Errorf(`when must be "now" or "idle"`))
		return
	}
	s.cancelScheduledRestart("") // an immediate restart supersedes it
	n, err := s.restartNow(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, map[string]int{"resuming": n})
}

// restartNow saves the hosted sessions and stops the server with
// ErrRestart a moment later (so a reply, and the hub's relay of it, goes
// out first).
func (s *Server) restartNow(ctx context.Context) (n int, err error) {
	ctx, end := tracing.Span(ctx, "collector.restart")
	defer func() { end(err) }()
	n, err = s.saveResumeList(ctx)
	if err != nil {
		return 0, fmt.Errorf("save resume list: %w", err)
	}
	log.Printf("restart requested; %d hosted sessions will be resumed", n)
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.restartAsked.Store(true)
		s.cancelFn()
	}()
	return n, nil
}
