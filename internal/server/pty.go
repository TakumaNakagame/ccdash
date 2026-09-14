package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"syscall"

	"github.com/takumanakagame/ccmanage/internal/attach"
)

// newPTYCommand builds the child for POST /pty/start. A variable so tests
// can substitute a shell instead of the real claude binary.
var newPTYCommand = attach.ViaShell

// handlePTY is the single mux entry for all /pty/* routes. It dispatches on
// method and path suffix so we can share the requireToken middleware wrapper.
func (s *Server) handlePTY(w http.ResponseWriter, r *http.Request) {
	// Path patterns under /pty/:
	//   GET    /pty/                 → handlePTYList
	//   POST   /pty/start            → handlePTYStart
	//   GET    /pty/{id}/stream      → handlePTYStream  (raw relay, fullscreen attach)
	//   GET    /pty/{id}/screen      → handlePTYScreen  (emulated frames, right pane)
	//   POST   /pty/{id}/resize      → handlePTYResize
	//   POST   /pty/{id}/register    → handlePTYRegister
	//   DELETE /pty/{id}             → handlePTYClose
	path := strings.TrimPrefix(r.URL.Path, "/pty/")
	path = strings.TrimSuffix(path, "/")

	if path == "" && r.Method == http.MethodGet {
		s.handlePTYList(w, r)
		return
	}
	if path == "start" && r.Method == http.MethodPost {
		s.handlePTYStart(w, r)
		return
	}

	// Remaining paths are /pty/{id}/{action} or /pty/{id}.
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "bad pty path", http.StatusBadRequest)
		return
	}
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case action == "stream" && r.Method == http.MethodGet:
		s.handlePTYStream(w, r, id)
	case action == "screen" && r.Method == http.MethodGet:
		s.handlePTYScreen(w, r, id)
	case action == "resize" && r.Method == http.MethodPost:
		s.handlePTYResize(w, r, id)
	case action == "register" && r.Method == http.MethodPost:
		s.handlePTYRegister(w, r, id)
	case action == "" && r.Method == http.MethodDelete:
		s.handlePTYClose(w, r, id)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// handlePTYStart creates a new PTY session or returns an existing alive one.
// Body: {"sessionId":"...", "cwd":"...", "resumeId":"...", "cols":N, "rows":N}
// If resumeId is empty a bare `claude` is spawned (new session). cols/rows
// size the PTY + emulator up front so the child's first render already fits
// the viewer; zero falls back to 80x24.
// Returns: {"ptyKey":"..."} — the key the TUI uses for subsequent calls.
func (s *Server) handlePTYStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
		ResumeID  string `json:"resumeId"`
		Cwd       string `json:"cwd"`
		Cols      int    `json:"cols"`
		Rows      int    `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	// If we already have an alive entry for this sessionID, reuse it.
	if req.SessionID != "" {
		s.ptyMu.Lock()
		entry, ok := s.ptyMap[req.SessionID]
		s.ptyMu.Unlock()
		if ok && entry.sess.Alive() {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"ptyKey": entry.ptyKey})
			return
		}
	}

	// Build the claude command.
	var args []string
	if req.ResumeID != "" {
		args = append(args, "--resume", req.ResumeID)
	}
	c := newPTYCommand(args...)
	if req.Cwd != "" {
		c.Dir = req.Cwd
	}
	c.Env = attach.SafeEnv()

	sess := attach.New(c)
	// The key is finalized after Start (we need the PID), but the entry
	// must exist before Start so the sink is wired for the first bytes.
	entry := newPTYEntry(sess, "", req.Cols, req.Rows)
	if err := sess.Start(); err != nil {
		http.Error(w, fmt.Sprintf("pty start: %v", err), http.StatusInternalServerError)
		return
	}
	entry.startPumps()

	// Use PID as the initial ptyKey (unique, known immediately).
	ptyKey := fmt.Sprintf("pid-%d", sess.PID())
	if req.SessionID != "" {
		ptyKey = req.SessionID
	}
	entry.ptyKey = ptyKey

	s.ptyMu.Lock()
	s.ptyMap[ptyKey] = entry
	s.ptyMu.Unlock()

	// Auto-remove when the child exits.
	go func() {
		<-sess.ChildExit()
		s.ptyMu.Lock()
		// Drop every alias that points at this entry (pid key + sessionID
		// alias) so GET /pty stops advertising it.
		for k, e := range s.ptyMap {
			if e == entry {
				delete(s.ptyMap, k)
			}
		}
		s.ptyMu.Unlock()
		log.Printf("pty: session %s exited", ptyKey)
	}()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"ptyKey": ptyKey})
}

// handlePTYStream upgrades the HTTP connection to a raw bidirectional PTY
// relay (fullscreen attach). Only one client can stream per entry at a
// time (409 if already taken). Screen viewers keep receiving frames while
// a raw client is attached; the emulator just stops answering queries.
func (s *Server) handlePTYStream(w http.ResponseWriter, r *http.Request, id string) {
	entry := s.lookupPTY(id)
	if entry == nil {
		http.Error(w, "pty not found", http.StatusNotFound)
		return
	}
	if !entry.sess.Alive() {
		http.Error(w, "pty session exited", http.StatusGone)
		return
	}
	if !entry.connected.CompareAndSwap(false, true) {
		http.Error(w, "pty already connected", http.StatusConflict)
		return
	}
	defer entry.connected.Store(false)

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("hijack: %v", err), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	// Send 101 Switching Protocols.
	_, _ = fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: pty-raw\r\nConnection: Upgrade\r\n\r\n")
	_ = brw.Flush()

	// Route PTY output to the client connection BEFORE resizing.
	// Setsize triggers SIGWINCH → claude full redraw; the sink must already
	// point at conn or the redraw bytes never reach the operator.
	entry.setRaw(conn)
	defer entry.setRaw(nil)

	// Read JSON handshake: {"rows":N,"cols":N}
	var hs struct {
		Rows uint16 `json:"rows"`
		Cols uint16 `json:"cols"`
	}
	if err := json.NewDecoder(brw).Decode(&hs); err == nil && hs.Rows > 0 && hs.Cols > 0 {
		entry.resize(int(hs.Cols), int(hs.Rows))
	}
	// Explicit SIGWINCH: Setsize may not always deliver SIGWINCH (e.g. when
	// the size hasn't changed). Sending it directly guarantees a full redraw.
	if p := entry.sess.Process(); p != nil {
		_ = p.Signal(syscall.SIGWINCH)
	}
	// The emulator already holds the current screen but the operator's
	// terminal is blank — ask the child to repaint (Ctrl+L) so they don't
	// stare at nothing until the next output.
	if f := entry.sess.Pty(); f != nil {
		_, _ = f.Write([]byte{0x0c})
	}

	// client → PTY: copy until conn closes or PTY dies.
	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		_, _ = io.Copy(entry.sess.Pty(), conn)
	}()

	select {
	case <-entry.sess.ChildExit():
	case <-copyDone:
	}
}

// handlePTYResize updates the PTY + emulator window size.
// Body: {"rows":N,"cols":N}
func (s *Server) handlePTYResize(w http.ResponseWriter, r *http.Request, id string) {
	entry := s.lookupPTY(id)
	if entry == nil {
		http.Error(w, "pty not found", http.StatusNotFound)
		return
	}
	var req struct {
		Rows uint16 `json:"rows"`
		Cols uint16 `json:"cols"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	entry.resize(int(req.Cols), int(req.Rows))
	w.WriteHeader(http.StatusNoContent)
}

// handlePTYRegister aliases a ptyKey (PID-based) to the real sessionID so
// subsequent TUI calls can look up the entry by sessionID. Called by TUI
// once discovery resolves the new session's id.
// Body: {"sessionId":"..."}
func (s *Server) handlePTYRegister(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" {
		http.Error(w, "bad json or missing sessionId", http.StatusBadRequest)
		return
	}

	s.ptyMu.Lock()
	entry, ok := s.ptyMap[id]
	if ok {
		s.ptyMap[req.SessionID] = entry
	}
	s.ptyMu.Unlock()

	if !ok {
		http.Error(w, "pty not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePTYClose terminates the PTY session and removes it from the map.
func (s *Server) handlePTYClose(w http.ResponseWriter, r *http.Request, id string) {
	s.ptyMu.Lock()
	entry, ok := s.ptyMap[id]
	if ok {
		for k, e := range s.ptyMap {
			if e == entry {
				delete(s.ptyMap, k)
			}
		}
	}
	s.ptyMu.Unlock()

	if !ok {
		http.Error(w, "pty not found", http.StatusNotFound)
		return
	}
	_ = entry.sess.Close()
	w.WriteHeader(http.StatusNoContent)
}

// handleShutdown triggers a graceful server shutdown.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	if s.cancelFn != nil {
		go s.cancelFn()
	}
}

// lookupPTY finds an entry by id, holding the lock only for the map read.
func (s *Server) lookupPTY(id string) *ptyEntry {
	s.ptyMu.Lock()
	e := s.ptyMap[id]
	s.ptyMu.Unlock()
	return e
}

// Ensure brw (bufio.ReadWriter from Hijack) satisfies io.Reader for JSON decode.
var _ io.Reader = (*bufio.ReadWriter)(nil)
