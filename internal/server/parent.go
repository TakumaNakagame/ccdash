package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/accounts"
)

// Parents, briefs and SendMessage names
// (docs/decisions/0009-orchestrator.md).

// handleAPIParent sets a session's parent, overwriting the one it had
// (ccdash mcp's adopt_session), or clears it with "" (release_session).
// POST /api/sessions/{id}/parent {"parent","parentHost"}
func (s *Server) handleAPIParent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Parent     string `json:"parent"`
		ParentHost string `json:"parentHost"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !validParent(body.Parent) || !validParent(body.ParentHost) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("parent / parentHost must be at most 128 of a-z, A-Z, 0-9, - and _"))
		return
	}
	if body.Parent == id {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("a session can't be its own parent"))
		return
	}
	if err := s.db.AdoptParent(r.Context(), id, body.Parent, body.ParentHost); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, nil)
}

// handleAPIChildrenSeen stamps when a session last listed its children
// (my_sessions through store.Remote; the local store writes the DB
// itself). Like /wait it is the caller's own bookkeeping and stays off the
// hub allowlist. POST /api/sessions/{id}/children-seen {"at": unix ms}
func (s *Server) handleAPIChildrenSeen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		At int64 `json:"at"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	at := time.Now()
	if body.At > 0 {
		at = time.UnixMilli(body.At)
	}
	if err := s.db.SetChildrenSeen(r.Context(), r.PathValue("id"), at); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, nil)
}

// handleAPIBrief returns what the session was asked to do when ccdash
// started it (oldest first; empty for a session started any other way).
// With ?messages=1 the answer is an object instead of the bare list —
// {"briefs": […], "messages": […]} — adding what other sessions told this
// one afterwards (message.go; text clipped), so a screen can show the
// whole story of what the session was asked. The bare list stays the
// default because older clients decode exactly that.
// GET /api/sessions/{id}/brief[?messages=1]
func (s *Server) handleAPIBrief(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bs, err := s.db.SessionBriefs(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if r.URL.Query().Get("messages") == "" {
		writeOK(w, bs)
		return
	}
	ms, err := s.db.SessionMessages(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for i := range ms {
		ms[i].Text = truncate(ms[i].Text, messageClip)
	}
	writeOK(w, map[string]any{"briefs": bs, "messages": ms})
}

// handleAPIPeers maps the running claudes' session ids to the name Claude
// Code's SendMessage reaches them by — the one field of
// ~/.claude/sessions/<pid>.json this ever serves. It changes per process,
// so it is read here and never stored. Not on the hub allowlist:
// SendMessage only works between sessions of one machine. GET /api/peers
func (s *Server) handleAPIPeers(w http.ResponseWriter, _ *http.Request) {
	writeOK(w, accounts.Peers())
}
