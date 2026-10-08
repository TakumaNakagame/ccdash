package server

// Restarting things without losing the hosted claude sessions:
//
//   - POST /pty/{key}/restart kills one hosted claude and starts
//     `claude --resume <session>` in its place (same key, cwd and size).
//   - POST /api/restart saves the hosted sessions to resume-on-start.json and
//     stops the server with ErrRestart, on which `ccdash server` re-execs
//     itself (only it sets CanRestart); the
//     TUI's "Restart ccdash" does the same save via POST /shutdown?resume=1
//     before it stops the collector and spawns a fresh one.
//   - On start, resumeSaved brings back what a recent save lists. The file
//     is consumed once and ignored when stale, so a reboot days later does
//     not suddenly launch claude everywhere.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/paths"
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
func (s *Server) hostedSessions(r *http.Request) []resumeEntry {
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
		sess, ok, err := s.db.GetSession(r.Context(), h.sid)
		if err != nil || !ok || sess.Cwd == "" {
			continue
		}
		out = append(out, resumeEntry{SessionID: h.sid, Cwd: sess.Cwd, Cols: h.cols, Rows: h.rows})
	}
	return out
}

// saveResumeList writes the hosted sessions for the next start to resume.
func (s *Server) saveResumeList(r *http.Request) (int, error) {
	list := s.hostedSessions(r)
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
	for i, e := range list {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		if _, _, _, err := s.startPTY(ptyStartReq{SessionID: e.SessionID, ResumeID: e.SessionID, Cwd: e.Cwd, Cols: e.Cols, Rows: e.Rows}); err != nil {
			log.Printf("resume %s: %v", e.SessionID, err)
			continue
		}
		log.Printf("resumed %s after restart", e.SessionID)
	}
}

// closeAllPTYs ends every hosted claude (before a re-exec).
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
	key, _, code, err := s.startPTY(ptyStartReq{SessionID: sid, ResumeID: sid, Cwd: sess.Cwd, Cols: cols, Rows: rows})
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	writeOK(w, map[string]string{"ptyKey": key})
}

// handleRestart: POST /api/restart — save the hosted sessions and re-exec the
// collector, which resumes them on start. 501 where the process can't
// re-exec itself (CanRestart unset).
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if !s.CanRestart || s.cancelFn == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("this collector cannot restart itself; restart it from where it runs"))
		return
	}
	n, err := s.saveResumeList(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("save resume list: %w", err))
		return
	}
	writeOK(w, map[string]int{"resuming": n})
	log.Printf("restart requested; %d hosted sessions will be resumed", n)
	go func() {
		time.Sleep(300 * time.Millisecond) // let the response (and the hub's relay of it) go out
		s.restartAsked.Store(true)
		s.cancelFn()
	}()
}
