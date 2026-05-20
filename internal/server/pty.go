package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/creack/pty"
	"github.com/takumanakagame/ccmanage/internal/attach"
)

// ptyEntry is one long-lived PTY session managed by the server. It survives
// TUI restarts; the TUI is just a thin relay while connected.
type ptyEntry struct {
	sess    *attach.Session
	ptyKey  string // always set — UUID at creation, then also registered under sessionID
	mu      sync.Mutex
	connected atomic.Bool // true while a TUI stream is active
}

// handlePTY is the single mux entry for all /pty/* routes. It dispatches on
// method and path suffix so we can share the requireToken middleware wrapper.
func (s *Server) handlePTY(w http.ResponseWriter, r *http.Request) {
	// Path patterns under /pty/:
	//   POST   /pty/start            → handlePTYStart
	//   GET    /pty/{id}/stream      → handlePTYStream
	//   POST   /pty/{id}/resize      → handlePTYResize
	//   POST   /pty/{id}/register    → handlePTYRegister
	//   DELETE /pty/{id}             → handlePTYClose
	path := strings.TrimPrefix(r.URL.Path, "/pty/")
	path = strings.TrimSuffix(path, "/")

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
// Body: {"sessionId":"...", "cwd":"...", "resumeId":"..."}
// If resumeId is empty a bare `claude` is spawned (new session).
// Returns: {"ptyKey":"..."} — the key the TUI uses for subsequent calls.
func (s *Server) handlePTYStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
		ResumeID  string `json:"resumeId"`
		Cwd       string `json:"cwd"`
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
	c := attach.ViaShell(args...)
	if req.Cwd != "" {
		c.Dir = req.Cwd
	}
	c.Env = attach.SafeEnv()

	sess := attach.New(c)
	if err := sess.Start(); err != nil {
		http.Error(w, fmt.Sprintf("pty start: %v", err), http.StatusInternalServerError)
		return
	}

	// Use PID as the initial ptyKey (unique, known immediately).
	ptyKey := fmt.Sprintf("pid-%d", sess.PID())
	if req.SessionID != "" {
		ptyKey = req.SessionID
	}

	entry := &ptyEntry{sess: sess, ptyKey: ptyKey}

	s.ptyMu.Lock()
	s.ptyMap[ptyKey] = entry
	s.ptyMu.Unlock()

	// Auto-remove when the child exits.
	go func() {
		<-sess.ChildExit()
		s.ptyMu.Lock()
		// Only delete if the entry is still ours (register may have added a
		// second alias; we leave the sessionID alias for the TUI to inspect).
		if e, ok := s.ptyMap[ptyKey]; ok && e == entry {
			delete(s.ptyMap, ptyKey)
		}
		s.ptyMu.Unlock()
		log.Printf("pty: session %s exited", ptyKey)
	}()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"ptyKey": ptyKey})
}

// handlePTYStream upgrades the HTTP connection to a raw bidirectional PTY
// relay. Only one TUI can stream per entry at a time (409 if already taken).
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

	// Read JSON handshake: {"rows":N,"cols":N}
	var hs struct {
		Rows uint16 `json:"rows"`
		Cols uint16 `json:"cols"`
	}
	if err := json.NewDecoder(brw).Decode(&hs); err == nil && hs.Rows > 0 && hs.Cols > 0 {
		if f := entry.sess.Pty(); f != nil {
			_ = pty.Setsize(f, &pty.Winsize{Rows: hs.Rows, Cols: hs.Cols})
		}
	}

	// Route PTY output to the client connection.
	entry.sess.SetSink(conn)
	defer entry.sess.SetSink(io.Discard)

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

// handlePTYResize updates the PTY window size.
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
	if f := entry.sess.Pty(); f != nil {
		_ = pty.Setsize(f, &pty.Winsize{Rows: req.Rows, Cols: req.Cols})
	}
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
		delete(s.ptyMap, id)
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
