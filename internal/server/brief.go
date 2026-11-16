package server

import (
	"net/http"
	"strings"
)

// The operator's notes for start_session briefs and each project's
// working directory (docs/decisions/0009-orchestrator.md §5). They are
// input to every child session, so only people write them: these routes
// are for the portal and `ccdash brief` — no MCP tool calls them, and the
// hub keeps them off the peer routes (a device token can't change them).
//
// On the hub allowlist they are person edits gated by attach_enabled
// (hubAllowed): the portal can already start a session with any prompt in
// any directory through /pty/start, so writing a note that later starts
// carry gives the hub no new power. They are deliberately NOT settings
// writes: the common brief has its own narrow route, and
// PUT /api/settings/{key} refuses its key (settings.LocalOnly), so
// "settings writes stay off the hub allowlist" still holds.

// handleAPIBriefCommon: GET /api/brief-common → {"brief": "..."}.
func (s *Server) handleAPIBriefCommon(w http.ResponseWriter, r *http.Request) {
	text, err := s.db.BriefCommon(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, map[string]string{"brief": text})
}

// handleAPIBriefCommonSet: PUT /api/brief-common {"brief": "..."} ("" clears).
func (s *Server) handleAPIBriefCommonSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Brief string `json:"brief"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.db.SetBriefCommon(r.Context(), body.Brief); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.handleAPIBriefCommon(w, r)
}

// handleAPIProjectBrief: POST /api/projects/brief {"project","brief"}
// ("" clears; the project is created when new).
func (s *Server) handleAPIProjectBrief(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Project string `json:"project"`
		Brief   string `json:"brief"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.db.SetProjectBrief(r.Context(), body.Project, body.Brief); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeOK(w, nil)
}

// handleAPIProjectDir: POST /api/projects/dir {"project","dir"} — an
// existing absolute directory of this machine, "" clears. Answers
// {"dir": "<cleaned path>"}.
func (s *Server) handleAPIProjectDir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Project string `json:"project"`
		Dir     string `json:"dir"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	dir, err := s.db.SetProjectDir(r.Context(), body.Project, body.Dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeOK(w, map[string]string{"dir": dir})
}

// handleAPIStartInfo tells ccdash mcp's start_session, before it sends a
// start with no cwd, whether this collector can find one: the project's
// directory, or a setup command that may name it. An older collector has
// no such route (404) and would start claude in its own directory, so the
// caller then insists on a cwd. GET /api/start-info?project=
func (s *Server) handleAPIStartInfo(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	dir := ""
	if project != "" {
		var err error
		if dir, _, err = s.db.ProjectStart(r.Context(), project); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeOK(w, map[string]any{
		"dir":   dir,
		"setup": strings.TrimSpace(s.startDefaults().SessionSetupCommand) != "",
	})
}
