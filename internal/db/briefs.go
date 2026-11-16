package db

import (
	"context"
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
	res, err := d.sql.ExecContext(ctx, `INSERT INTO briefs (session_id, pty_key, parent, project, cwd, text, created_at)
		VALUES (NULLIF(?,''), ?, ?, ?, ?, ?, ?)`, b.SessionID, b.PtyKey, b.Parent, b.Project, b.Cwd, b.Text, at.Unix())
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
		COALESCE(project,''), COALESCE(cwd,''), text, created_at FROM briefs WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Brief{}
	for rows.Next() {
		var b model.Brief
		var at int64
		if err := rows.Scan(&b.ID, &b.SessionID, &b.PtyKey, &b.Parent, &b.Project, &b.Cwd, &b.Text, &at); err != nil {
			return nil, err
		}
		b.CreatedAt = time.Unix(at, 0).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}
