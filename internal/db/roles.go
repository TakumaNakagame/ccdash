package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// Orchestrator roles (docs/decisions/0009-orchestrator.md §7, §10, §12).
//
// sessions.role is "" / orchestrator / project-orchestrator. A project
// orchestrator watches the project it is in, and projects.orchestrator
// points back at it — one per project on this machine. Only people set
// roles (POST /api/sessions/{id}/role, `ccdash role`); a spawn carries the
// role it was started with (applyPTYProjects).

// ErrNoSession is returned by SetRole for an unknown session id.
var ErrNoSession = errors.New("no such session")

// SetRole gives the session a role ("" takes it away). A project
// orchestrator must be in a project; the project's previous orchestrator
// loses the role. With from, the role moves over from that session in one
// step: from loses its role and its children on this machine get the
// session as their parent — and with role "" the session takes whatever
// role from had.
func (d *DB) SetRole(ctx context.Context, sessionID, role, from string) (model.RoleChange, error) {
	var out model.RoleChange
	if !model.ValidRole(role) {
		return out, fmt.Errorf("role must be %q, %q or \"\"", model.RoleOrchestrator, model.RoleProjectOrchestrator)
	}
	if from == sessionID {
		return out, fmt.Errorf("a session can't take over from itself")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	get := func(id string) (role, project string, err error) {
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(role,''), COALESCE(project,'') FROM sessions WHERE session_id = ?`, id).Scan(&role, &project)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrNoSession
		}
		return role, project, err
	}
	_, project, err := get(sessionID)
	if err != nil {
		return out, err
	}
	fromRole := ""
	if from != "" {
		var fromProject string
		fromRole, fromProject, err = get(from)
		if errors.Is(err, ErrNoSession) {
			return out, fmt.Errorf("from: no such session on this machine")
		} else if err != nil {
			return out, err
		}
		if role == "" {
			role = fromRole
		}
		if role == "" {
			return out, fmt.Errorf("the session to take over from is not an orchestrator; say which role to take")
		}
		if role == model.RoleProjectOrchestrator && project == "" && fromProject != "" {
			return out, fmt.Errorf("put the session into project %q first: a project's orchestrator watches the project it is in", fromProject)
		}
	}
	if role == model.RoleProjectOrchestrator && project == "" {
		return out, fmt.Errorf("the session is in no project: a project's orchestrator watches the project it is in")
	}
	drop := func(id string) error {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET role = NULL WHERE session_id = ?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE projects SET orchestrator = NULL WHERE orchestrator = ?`, id)
		return err
	}
	replaced := map[string]bool{}
	// Whatever the session was before goes first (its project pointer too).
	if err := drop(sessionID); err != nil {
		return out, err
	}
	if from != "" {
		if fromRole != "" {
			replaced[from] = true
		}
		if err := drop(from); err != nil {
			return out, err
		}
		res, err := tx.ExecContext(ctx, `UPDATE sessions SET parent = ?1, parent_host = NULL WHERE parent = ?2 AND session_id <> ?1`, sessionID, from)
		if err != nil {
			return out, err
		}
		n, _ := res.RowsAffected()
		out.Moved = int(n)
	}
	if role == model.RoleProjectOrchestrator {
		var prev string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(orchestrator,'') FROM projects WHERE name = ?`, project).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		if prev != "" && prev != sessionID {
			if err := drop(prev); err != nil {
				return out, err
			}
			replaced[prev] = true
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET orchestrator = ? WHERE name = ?`, sessionID, project); err != nil {
			return out, err
		}
		out.Project = project
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET role = NULLIF(?, '') WHERE session_id = ?`, role, sessionID); err != nil {
		return out, err
	}
	out.Role = role
	for id := range replaced {
		out.Replaced = append(out.Replaced, id)
	}
	return out, tx.Commit()
}

// SetProjectOrchestratorModel sets the model and effort a project's
// orchestrator starts with ("" = the orchestrator_model / _effort
// settings). The caller validates both. The project is created when new.
func (d *DB) SetProjectOrchestratorModel(ctx context.Context, project, modelName, effort string) error {
	project = strings.TrimSpace(project)
	if project == "" {
		return fmt.Errorf("project is required")
	}
	if err := d.ensureProject(ctx, project); err != nil {
		return err
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE projects SET orchestrator_model = NULLIF(?, ''), orchestrator_effort = NULLIF(?, '') WHERE name = ?`, modelName, effort, project)
	return err
}

// RoleOf returns a session's role and, for a project orchestrator, the
// project it watches with that project's model / effort overrides. All ""
// for an unknown session or one without a role.
func (d *DB) RoleOf(ctx context.Context, sessionID string) (role, project, modelName, effort string, err error) {
	err = d.sql.QueryRowContext(ctx, `SELECT COALESCE(s.role,''), COALESCE(s.project,''),
		COALESCE(p.orchestrator_model,''), COALESCE(p.orchestrator_effort,'')
		FROM sessions s LEFT JOIN projects p ON p.name = s.project WHERE s.session_id = ?`, sessionID).Scan(&role, &project, &modelName, &effort)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", "", nil
	}
	if role != model.RoleProjectOrchestrator {
		project, modelName, effort = "", "", ""
	}
	return role, project, modelName, effort, err
}

// ProjectOrchestratorModel returns a project's orchestrator model / effort
// overrides ("" when unset or no such project).
func (d *DB) ProjectOrchestratorModel(ctx context.Context, project string) (modelName, effort string, err error) {
	err = d.sql.QueryRowContext(ctx, `SELECT COALESCE(orchestrator_model,''), COALESCE(orchestrator_effort,'') FROM projects WHERE name = ?`, project).Scan(&modelName, &effort)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return modelName, effort, err
}

// Orchestrator returns the session id of this machine's global
// orchestrator that is not archived, the most recently active when there
// are several ("" = none).
func (d *DB) Orchestrator(ctx context.Context) (string, error) {
	var id string
	err := d.sql.QueryRowContext(ctx, `SELECT session_id FROM sessions WHERE role = ? AND COALESCE(archived,0) = 0
		ORDER BY last_seen DESC LIMIT 1`, model.RoleOrchestrator).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
