package server

import (
	"net/http"
	"strconv"

	"github.com/takumanakagame/ccmanage/internal/usage"
)

// handleAPISessionUsage: tokens and API-price estimate of one session,
// subagents included. GET /api/sessions/{id}/usage
func (s *Server) handleAPISessionUsage(w http.ResponseWriter, r *http.Request) {
	sess, ok, err := s.db.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok || sess.TranscriptPath == "" {
		http.NotFound(w, r)
		return
	}
	writeOK(w, usage.Default.ForSession(sess.TranscriptPath))
}

// handleAPIUsage: usage across sessions for the last ?days= days (default 7).
// GET /api/usage
func (s *Server) handleAPIUsage(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 90 {
		days = 7
	}
	paths := map[string]string{}
	for _, archived := range []bool{false, true} {
		ss, err := s.db.ListSessions(r.Context(), archived)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		for _, x := range ss {
			if x.TranscriptPath != "" {
				paths[x.SessionID] = x.TranscriptPath
			}
		}
	}
	writeOK(w, usage.Default.Summarize(paths, days))
}
