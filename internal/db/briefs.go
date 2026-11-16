package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// Parents and briefs (docs/decisions/0009-orchestrator.md).

// SetParent records which session started this one (ccdash mcp's
// start_session, through POST /pty/start). Like the origin, the first
// parent sticks: a spawn never moves a session that already has one.
func (d *DB) SetParent(ctx context.Context, sessionID, parent, parentHost string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE sessions SET parent = ?, parent_host = ?
		WHERE session_id = ? AND COALESCE(parent,'') = ''`, parent, parentHost, sessionID)
	return err
}

// AdoptParent sets the session's parent whatever it was (adopt_session),
// or clears it with parent "" (release_session).
func (d *DB) AdoptParent(ctx context.Context, sessionID, parent, parentHost string) error {
	if parent == "" {
		parentHost = ""
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE sessions SET parent = ?, parent_host = ? WHERE session_id = ?`, parent, parentHost, sessionID)
	return err
}

// SetChildrenSeen stamps when the session last listed its children
// (my_sessions); it only ever moves forward.
func (d *DB) SetChildrenSeen(ctx context.Context, sessionID string, at time.Time) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE sessions SET children_seen_at = MAX(COALESCE(children_seen_at,0), ?) WHERE session_id = ?`, at.UnixMilli(), sessionID)
	return err
}

// InsertBrief keeps what a new spawn was asked to do. The session id is
// not known yet (b.SessionID is usually empty); LinkBrief fills it in.
// The caller redacts b.Text.
func (d *DB) InsertBrief(ctx context.Context, b model.Brief) (int64, error) {
	at := b.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	res, err := d.sql.ExecContext(ctx, `INSERT INTO briefs (session_id, pty_key, parent, project, cwd, text, created_at, notes, own_len)
		VALUES (NULLIF(?,''), ?, ?, ?, ?, ?, ?, NULLIF(?,''), NULLIF(?,0))`,
		b.SessionID, b.PtyKey, b.Parent, b.Project, b.Cwd, b.Text, at.Unix(), strings.Join(b.Notes, ","), b.OwnLen)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// LinkBrief ties a brief to the session its spawn turned out to be. A brief
// that already has a session keeps it.
func (d *DB) LinkBrief(ctx context.Context, id int64, sessionID string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE briefs SET session_id = ? WHERE id = ? AND session_id IS NULL`, sessionID, id)
	return err
}

// SessionBriefs returns the briefs of one session, oldest first.
func (d *DB) SessionBriefs(ctx context.Context, sessionID string) ([]model.Brief, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, COALESCE(session_id,''), COALESCE(pty_key,''), COALESCE(parent,''),
		COALESCE(project,''), COALESCE(cwd,''), text, created_at, COALESCE(notes,''), COALESCE(own_len,0)
		FROM briefs WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Brief{}
	for rows.Next() {
		var b model.Brief
		var at int64
		var notes string
		if err := rows.Scan(&b.ID, &b.SessionID, &b.PtyKey, &b.Parent, &b.Project, &b.Cwd, &b.Text, &at, &notes, &b.OwnLen); err != nil {
			return nil, err
		}
		if notes != "" {
			b.Notes = strings.Split(notes, ",")
		}
		b.CreatedAt = time.Unix(at, 0).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

// The operator's notes for start_session briefs
// (docs/decisions/0009-orchestrator.md §5). People write them — the
// portal, `ccdash brief` — never a session: they become input to every
// child session.

// keyBriefCommon is the settings-table row of the common brief. It is not
// one of settings.AllSpecs and PUT /api/settings/{key} refuses it: the
// only ways in are SetBriefCommon's callers.
const keyBriefCommon = "brief_common"

// BriefCommon returns the text appended to every start_session brief on
// this machine ("" = none).
func (d *DB) BriefCommon(ctx context.Context) (string, error) {
	return d.GetSetting(ctx, keyBriefCommon)
}

// SetBriefCommon replaces the common brief ("" clears it).
func (d *DB) SetBriefCommon(ctx context.Context, text string) error {
	text, err := cleanBrief(text)
	if err != nil {
		return err
	}
	return d.SetSetting(ctx, keyBriefCommon, text)
}

func cleanBrief(text string) (string, error) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if len(text) > model.BriefMax {
		return "", fmt.Errorf("the note is too long (%d bytes; at most %d)", len(text), model.BriefMax)
	}
	if strings.ContainsRune(text, 0) {
		return "", fmt.Errorf("the note must be plain text")
	}
	return text, nil
}

// SetProjectBrief sets the note appended to start_session briefs for one
// project ("" clears it). The project is created when it doesn't exist
// yet, so a machine can be prepared before its first session there.
func (d *DB) SetProjectBrief(ctx context.Context, project, brief string) error {
	project = strings.TrimSpace(project)
	if project == "" {
		return fmt.Errorf("project is required")
	}
	brief, err := cleanBrief(brief)
	if err != nil {
		return err
	}
	if err := d.ensureProject(ctx, project); err != nil {
		return err
	}
	_, err = d.sql.ExecContext(ctx, `UPDATE projects SET brief = NULLIF(?, '') WHERE name = ?`, brief, project)
	return err
}

// SetProjectDir sets where this machine works on a project: an existing
// absolute directory ("" clears it). Returns the cleaned path.
func (d *DB) SetProjectDir(ctx context.Context, project, dir string) (string, error) {
	project, dir = strings.TrimSpace(project), strings.TrimSpace(dir)
	if project == "" {
		return "", fmt.Errorf("project is required")
	}
	if dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("dir must be an absolute path, got %q", dir)
		}
		dir = filepath.Clean(dir)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("%s is not a directory on this machine", dir)
		}
	}
	if err := d.ensureProject(ctx, project); err != nil {
		return "", err
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE projects SET dir = NULLIF(?, '') WHERE name = ?`, dir, project)
	return dir, err
}

// ProjectStart returns what a start_session in project draws from its
// row: the directory and the note ("" when unset or no such project).
func (d *DB) ProjectStart(ctx context.Context, project string) (dir, brief string, err error) {
	err = d.sql.QueryRowContext(ctx, `SELECT COALESCE(dir,''), COALESCE(brief,'') FROM projects WHERE name = ?`, project).Scan(&dir, &brief)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return dir, brief, err
}
