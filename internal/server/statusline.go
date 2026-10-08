package server

// The status JSON Claude Code hands its statusLine command (model, cost,
// context window, lines changed, rate limits …), relayed here by ccdash's
// statusline.sh (internal/hookcfg/statusline.go). Kept in memory, latest per
// session: it is refreshed on every UI update of a running claude, so there
// is nothing worth persisting.

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/takumanakagame/ccmanage/internal/redact"
)

type statusSnap struct {
	Data json.RawMessage `json:"data"`
	At   time.Time       `json:"at"`
}

// statusLineMax bounds the map: sessions that haven't reported for a day
// are dropped on the next write.
const statusLineMaxAge = 24 * time.Hour

// handleStatusLine: POST /hooks/statusline — the relayed status JSON.
func (s *Server) handleStatusLine(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var p struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(raw, &p) != nil || p.SessionID == "" {
		writeOK(w, nil) // nothing to file it under; don't make the relay noisy
		return
	}
	now := time.Now()
	s.statusMu.Lock()
	if s.statusLines == nil {
		s.statusLines = map[string]statusSnap{}
	}
	for k, v := range s.statusLines {
		if now.Sub(v.At) > statusLineMaxAge {
			delete(s.statusLines, k)
		}
	}
	s.statusLines[p.SessionID] = statusSnap{Data: redact.JSON(raw), At: now}
	s.statusMu.Unlock()
	writeOK(w, nil)
}

// handleAPIStatusLine: GET /api/sessions/{id}/statusline — the latest status
// JSON and when it arrived; 404 when the session never reported one.
func (s *Server) handleAPIStatusLine(w http.ResponseWriter, r *http.Request) {
	s.statusMu.Lock()
	snap, ok := s.statusLines[r.PathValue("id")]
	s.statusMu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeOK(w, snap)
}
