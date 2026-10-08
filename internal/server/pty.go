package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/takumanakagame/ccmanage/internal/attach"
	"github.com/takumanakagame/ccmanage/internal/procmap"
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
	//   POST   /pty/{id}/input       → handlePTYInput   (text / keys, no exclusive attach)
	//   GET    /pty/{id}/text        → handlePTYText    (plain-text screen snapshot)
	//   POST   /pty/{id}/restart     → handlePTYRestart (kill + claude --resume, restart.go)
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
	case action == "input" && r.Method == http.MethodPost:
		s.handlePTYInput(w, r, id)
	case action == "text" && r.Method == http.MethodGet:
		s.handlePTYText(w, r, id)
	case action == "restart" && r.Method == http.MethodPost:
		s.handlePTYRestart(w, r, id)
	case action == "" && r.Method == http.MethodDelete:
		s.handlePTYClose(w, r, id)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// ptyStartReq is the body of POST /pty/start (and what startPTY takes).
type ptyStartReq struct {
	SessionID string `json:"sessionId"`
	ResumeID  string `json:"resumeId"`
	Cwd       string `json:"cwd"`
	Prompt    string `json:"prompt"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	// PermissionMode is passed as --permission-mode. bypassPermissions
	// / dontAsk are not offered: they'd let a remote start skip every
	// safety prompt.
	PermissionMode string `json:"permissionMode"`
}

// handlePTYStart creates a new PTY session or returns an existing alive one.
// Body: {"sessionId":"...", "cwd":"...", "resumeId":"...", "prompt":"...", "cols":N, "rows":N}
// If resumeId is empty a bare `claude` is spawned (new session); a
// non-empty prompt becomes its first message (`claude "<prompt>"`), which
// is how the TUI's skill picker launches `/<skill>` in a fresh session. cols/rows
// size the PTY + emulator up front so the child's first render already fits
// the viewer; zero falls back to 80x24.
// Returns: {"ptyKey":"..."} — the key the TUI uses for subsequent calls.
func (s *Server) handlePTYStart(w http.ResponseWriter, r *http.Request) {
	var req ptyStartReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	key, tag, code, err := s.startPTY(req)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if tag == 0 { // an existing alive entry
		_ = json.NewEncoder(w).Encode(map[string]string{"ptyKey": key})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ptyKey": key, "tag": tag})
}

// startPTY spawns claude in a new PTY (see handlePTYStart). It returns the
// PTY key and hook tag — tag 0 when an alive entry for req.SessionID
// already existed — or an HTTP status and error.
func (s *Server) startPTY(req ptyStartReq) (string, int, int, error) {
	switch req.PermissionMode {
	case "", "manual", "acceptEdits", "auto", "plan":
	default:
		return "", 0, http.StatusBadRequest, fmt.Errorf("permissionMode must be manual, acceptEdits, auto or plan")
	}

	// If we already have an alive entry for this sessionID, reuse it.
	if req.SessionID != "" {
		s.ptyMu.Lock()
		entry, ok := s.ptyMap[req.SessionID]
		s.ptyMu.Unlock()
		if ok && entry.sess.Alive() {
			return entry.ptyKey, 0, 0, nil
		}
	}

	// Build the claude command.
	var args []string
	if req.ResumeID != "" {
		args = append(args, "--resume", req.ResumeID)
	}
	if req.PermissionMode != "" {
		args = append(args, "--permission-mode", req.PermissionMode)
	}
	if strings.HasPrefix(req.Prompt, "-") {
		// claude would parse it as a flag, not a message.
		return "", 0, http.StatusBadRequest, fmt.Errorf("prompt must not start with '-'")
	}
	if req.Prompt != "" {
		args = append(args, req.Prompt)
	}
	c := newPTYCommand(args...)
	if req.Cwd != "" {
		c.Dir = req.Cwd
	}
	s.ptyMu.Lock()
	s.ptyTagSeq++
	tag := ptyTagBase + s.ptyTagSeq
	s.ptyMu.Unlock()
	// The installed hooks forward $CCDASH_WRAPPER_PID as a header, so the
	// child's first hook tells us which session this PTY holds. (A later
	// duplicate wins in exec's env dedup, overriding any inherited value.)
	c.Env = append(attach.SafeEnv(), fmt.Sprintf("CCDASH_WRAPPER_PID=%d", tag))

	sess := attach.New(c)
	// The key is finalized after Start (we need the PID), but the entry
	// must exist before Start so the sink is wired for the first bytes.
	entry := newPTYEntry(sess, "", req.Cols, req.Rows)
	entry.tag = tag
	if err := sess.Start(); err != nil {
		return "", 0, http.StatusInternalServerError, fmt.Errorf("pty start: %v", err)
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
	return ptyKey, tag, 0, nil
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

// handlePTYInput writes to the PTY without taking the exclusive raw stream,
// so the portal's chat view can type while a terminal (TUI live pane, a
// fullscreen attach) is also watching. Body: {"text":"...","submit":bool,
// "keys":"..."}. text is delivered as a bracketed paste when it spans lines
// (Claude Code keeps it as one message instead of submitting at the first
// newline); submit presses Enter after it; keys are raw bytes (Esc, arrows,
// digits for a menu) written as-is.
func (s *Server) handlePTYInput(w http.ResponseWriter, r *http.Request, id string) {
	entry := s.lookupPTY(id)
	if entry == nil {
		http.Error(w, "pty not found", http.StatusNotFound)
		return
	}
	var req struct {
		Text   string `json:"text"`
		Submit bool   `json:"submit"`
		Keys   string `json:"keys"`
		Paste  bool   `json:"paste"` // force bracketed paste (e.g. an image path to attach)
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	f := entry.sess.Pty()
	if f == nil || !entry.sess.Alive() {
		http.Error(w, "pty session exited", http.StatusGone)
		return
	}
	if req.Keys != "" {
		_, _ = f.Write([]byte(req.Keys))
	}
	if req.Text != "" {
		text := strings.ReplaceAll(req.Text, "\r\n", "\n")
		if req.Paste || strings.Contains(text, "\n") {
			text = "\x1b[200~" + text + "\x1b[201~"
		}
		_, _ = f.Write([]byte(text))
	}
	if req.Submit {
		if req.Text != "" {
			// Let the TUI finish ingesting the text first; an Enter that
			// lands mid-paste is swallowed as a literal newline.
			time.Sleep(120 * time.Millisecond)
		}
		_, _ = f.Write([]byte("\r"))
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePTYText returns the emulated screen as plain text rows (styles
// stripped) plus the cursor, for the portal to show a TUI prompt (a
// permission dialog, a menu) that has no transcript representation.
func (s *Server) handlePTYText(w http.ResponseWriter, r *http.Request, id string) {
	entry := s.lookupPTY(id)
	if entry == nil {
		http.Error(w, "pty not found", http.StatusNotFound)
		return
	}
	rows, cols, height, cur := entry.snapshot()
	for i, row := range rows {
		rows[i] = strings.TrimRight(ansi.Strip(row), " ")
	}
	writeOK(w, map[string]any{"rows": rows, "cols": cols, "height": height, "cursorX": cur.X, "cursorY": cur.Y, "alive": entry.sess.Alive()})
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

// ptyTagBase is where the per-PTY hook tags start. They travel in the
// X-Ccdash-Wrapper-Pid slot (the only integer the installed hooks already
// forward), so they sit far above any real PID to stay distinguishable.
const ptyTagBase = 1 << 30

// aliasPTYByTag registers sessionID as a key for the PTY spawned with tag.
// No-op when the tag is unknown (e.g. the PTY already exited).
func (s *Server) aliasPTYByTag(tag int, sessionID string) {
	if sessionID == "" {
		return
	}
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	if _, ok := s.ptyMap[sessionID]; ok {
		return
	}
	for _, e := range s.ptyMap {
		if e.tag == tag {
			s.ptyMap[sessionID] = e
			return
		}
	}
}

// aliasPTYsByParent keys still-unaliased `n` spawns (pid-<shell pid>) by
// their session ID, found by matching a running claude's parent PID to
// the PTY's shell. This is the hook-less path: without ccdash's hooks
// installed, the tag in aliasPTYByTag never arrives, but claude still
// writes ~/.claude/sessions/<pid>.json (procs) right at startup — well
// before its first prompt creates the transcript discovery lists. When the
// shell execs claude (`$SHELL -c 'claude …'` with a single command), claude
// keeps the shell's PID, so a claude whose own PID is the PTY's matches too.
func (s *Server) aliasPTYsByParent(ctx context.Context, procs map[string]procmap.Entry) {
	s.ptyMu.Lock()
	pending := map[int]*ptyEntry{} // shell pid → entry, only if no alias yet
	for k, e := range s.ptyMap {
		if strings.HasPrefix(k, "pid-") && !e.exited.Load() {
			pending[e.sess.PID()] = e
		}
	}
	for k, e := range s.ptyMap {
		if !strings.HasPrefix(k, "pid-") {
			delete(pending, e.sess.PID())
		}
	}
	s.ptyMu.Unlock()
	if len(pending) == 0 || len(procs) == 0 {
		return
	}
	parents := parentPIDs(ctx)
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	for sid, pe := range procs {
		if _, ok := s.ptyMap[sid]; ok || sid == "" {
			continue
		}
		if e, ok := pending[pe.PID]; ok {
			s.ptyMap[sid] = e
		} else if e, ok := pending[parents[pe.PID]]; ok {
			s.ptyMap[sid] = e
		}
	}
}

// parentPIDs returns pid → ppid for every process, via one `ps` call
// (portable across darwin and linux). Empty on failure.
func parentPIDs(ctx context.Context) map[int]int {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=").Output()
	m := map[int]int{}
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			m[pid] = ppid
		}
	}
	return m
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

// handleShutdown triggers a graceful server shutdown. With ?resume=1 the
// hosted sessions are saved first, for the next collector to resume (the
// TUI's "Restart ccdash"; see restart.go).
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Query().Get("resume") == "1" {
		if n, err := s.saveResumeList(r); err != nil {
			log.Printf("shutdown: save resume list: %v", err)
		} else {
			log.Printf("shutdown: %d hosted sessions saved for resume", n)
		}
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
