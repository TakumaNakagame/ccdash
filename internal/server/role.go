package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/TakumaNakagame/ccdash/internal/db"
	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/settings"
	"github.com/TakumaNakagame/ccdash/internal/tunnel"
)

// Orchestrator roles (docs/decisions/0009-orchestrator.md §7, §10, §12).
//
// A session whose row says orchestrator / project-orchestrator is started
// and resumed differently, every time, by whatever path (a fresh start with
// "role", POST /pty/{key}/restart, the resume after a collector restart,
// resume_session, the portal's resume, a message that wakes it): all of
// them end in spawnPTY, which asks roleFor.
//
//   - CCDASH_ROLE (and CCDASH_ROLE_PROJECT) in the child's environment: its
//     ccdash mcp reads it at initialize and adds the role rules to its
//     instructions. attach.SafeEnv strips an inherited one, so the role
//     never leaks to a session the orchestrator's collector hosts.
//   - --disallowedTools from the orchestrator_deny setting: instructions
//     alone did not keep an orchestrator from doing the work itself.
//   - --model / --effort from the role (the project's override, else the
//     orchestrator_model / orchestrator_effort settings) — on a resume too,
//     unlike an ordinary session: the role decides, not what the session
//     last ran with.
//
// Only people give a role: POST /api/sessions/{id}/role (the portal,
// `ccdash role`), or "role" on a person's /pty/start. An MCP start
// ("orchestrated") can never carry one, a peer device neither, and no MCP
// tool sets one.

// roleStart is what a role adds to one start.
type roleStart struct {
	role    string
	project string // the project a project orchestrator watches
	model   string // the project's override ("" = the setting)
	effort  string
	deny    []string
}

// checkStartRole validates "role" on a POST /pty/start and says who may
// send it. A resume drops it: the row decides.
func (s *Server) checkStartRole(r *http.Request, req *ptyStartReq) (int, error) {
	if req.ResumeID != "" {
		req.Role = ""
		return 0, nil
	}
	req.Role = strings.TrimSpace(req.Role)
	if req.Role == "" {
		return 0, nil
	}
	if !model.IsOrchestrator(req.Role) {
		return http.StatusBadRequest, fmt.Errorf("role must be %q or %q", model.RoleOrchestrator, model.RoleProjectOrchestrator)
	}
	// A session must not be able to make an orchestrator: start_session
	// marks its starts, and another machine's ccdash mcp reaches this
	// route as a peer (the hub names the device; it refuses the field
	// itself too, hub/peer.go).
	if req.Orchestrated {
		return http.StatusForbidden, fmt.Errorf("a session started by another session can't be given a role: only the operator makes an orchestrator (the portal, or `ccdash role`)")
	}
	if r.Header.Get(tunnel.HeaderPeerID) != "" {
		return http.StatusForbidden, fmt.Errorf("another device can't start an orchestrator here: only the operator gives a role")
	}
	if req.Role == model.RoleProjectOrchestrator && strings.TrimSpace(req.Project) == "" {
		return http.StatusBadRequest, fmt.Errorf("a project's orchestrator needs its project: pass project")
	}
	return 0, nil
}

// roleFor is the role a start runs under: the request's for a new session,
// the row's for a resume. The zero value for everything else.
func (s *Server) roleFor(req ptyStartReq) roleStart {
	var rs roleStart
	if s.db == nil {
		return rs
	}
	ctx := context.Background()
	if req.ResumeID != "" {
		role, project, m, e, err := s.db.RoleOf(ctx, req.ResumeID)
		if err != nil || !model.IsOrchestrator(role) {
			return rs
		}
		rs = roleStart{role: role, project: project, model: m, effort: e}
	} else if model.IsOrchestrator(req.Role) {
		rs.role = req.Role
		if rs.role == model.RoleProjectOrchestrator {
			rs.project = strings.TrimSpace(req.Project)
			rs.model, rs.effort, _ = s.db.ProjectOrchestratorModel(ctx, rs.project)
		}
	} else {
		return rs
	}
	// The setting is validated on the way in, and settings.Load drops a
	// value edited into the table by hand that isn't a list of tool names
	// (the default list then applies); it is parsed once more here, so
	// nothing but tool names ever reaches the command line.
	rs.deny, _ = settings.ParseDeny(s.startDefaults().OrchestratorDeny)
	return rs
}

// env is what the child's ccdash mcp reads its role from.
func (rs roleStart) env() []string {
	if rs.role == "" {
		return nil
	}
	out := []string{"CCDASH_ROLE=" + rs.role}
	if rs.project != "" {
		out = append(out, "CCDASH_ROLE_PROJECT="+rs.project)
	}
	return out
}

// roleArgs is modelArgs for an orchestrator, plus the tools taken away:
// what the request names wins, then the project's override, then the
// orchestrator_model / orchestrator_effort settings — for a resume as much
// as for a new session, and never default_model / default_effort.
//
// --disallowedTools takes "<tools...>": written as two arguments it would
// swallow whatever follows, the prompt included (claude 2.1.327 says so
// itself), so it goes as one argument, --disallowedTools=a,b,c.
func roleArgs(req ptyStartReq, cfg settings.Settings, rs roleStart) ([]string, error) {
	if req.Model != "" && !settings.ValidModel(req.Model) {
		return nil, fmt.Errorf("model must be an alias or a model id (letters, digits and . _ - : [ ], at most 64, starting with a letter or digit)")
	}
	if !settings.ValidEffort(req.Effort) {
		return nil, fmt.Errorf("effort must be one of %s", strings.Join(settings.Efforts, ", "))
	}
	var args []string
	if len(rs.deny) > 0 {
		args = append(args, "--disallowedTools="+strings.Join(rs.deny, ","))
	}
	pick := func(vals ...string) string {
		for _, v := range vals {
			if v != "" {
				return v
			}
		}
		return ""
	}
	if m := pick(req.Model, rs.model, cfg.OrchestratorModel); m != "" && settings.ValidModel(m) {
		args = append(args, "--model", m)
	}
	if e := pick(req.Effort, rs.effort, cfg.OrchestratorEffort); e != "" && settings.ValidEffort(e) {
		args = append(args, "--effort", e)
	}
	return args, nil
}

// hostedAlive reports whether session id's claude runs in one of this
// collector's PTYs right now.
func (s *Server) hostedAlive(id string) bool {
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	e := s.ptyMap[id]
	return e != nil && e.sess.Alive()
}

// personOnly refuses a request another device made through the hub: roles
// are given by the operator (the portal's login, or this machine's own
// CLI), never by a device token.
func personOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(tunnel.HeaderPeerID) != "" {
		writeErr(w, http.StatusForbidden, fmt.Errorf("only the operator changes roles, not another device"))
		return false
	}
	return true
}

// handleAPIRole gives a session a role, takes it away (""), or — with
// "from" — hands the role and the children of another session over to it.
// The reply says whether a restart is needed for it to take effect: the
// rules, the tools and the model are applied when claude starts.
// POST /api/sessions/{id}/role {"role","from"} → model.RoleChange
func (s *Server) handleAPIRole(w http.ResponseWriter, r *http.Request) {
	if !personOnly(w, r) {
		return
	}
	id := r.PathValue("id")
	var body struct {
		Role string `json:"role"`
		From string `json:"from"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	body.Role, body.From = strings.TrimSpace(body.Role), strings.TrimSpace(body.From)
	if !validParent(body.From) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("from must be a session id"))
		return
	}
	ch, err := s.db.SetRole(r.Context(), id, body.Role, body.From)
	switch {
	case errors.Is(err, db.ErrNoSession):
		writeErr(w, http.StatusNotFound, err)
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ch.RestartNeeded = s.hostedAlive(id)
	if body.From != "" {
		ch.RestartFrom = s.hostedAlive(body.From)
	}
	if ch.Replaced == nil {
		ch.Replaced = []string{}
	}
	writeOK(w, ch)
}

// handleAPIProjectOrchestrator sets the model and effort a project's
// orchestrator runs with ("" = the orchestrator_model / orchestrator_effort
// settings). POST /api/projects/orchestrator {"project","model","effort"}
func (s *Server) handleAPIProjectOrchestrator(w http.ResponseWriter, r *http.Request) {
	if !personOnly(w, r) {
		return
	}
	var body struct {
		Project string `json:"project"`
		Model   string `json:"model"`
		Effort  string `json:"effort"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	body.Model, body.Effort = strings.TrimSpace(body.Model), strings.TrimSpace(body.Effort)
	if !settings.ValidModel(body.Model) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("model must be an alias or a model id (letters, digits and . _ - : [ ], at most 64, starting with a letter or digit)"))
		return
	}
	if !settings.ValidEffort(body.Effort) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("effort must be one of %s", strings.Join(settings.Efforts, ", ")))
		return
	}
	if err := s.db.SetProjectOrchestratorModel(r.Context(), body.Project, body.Model, body.Effort); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeOK(w, map[string]string{"model": body.Model, "effort": body.Effort})
}
