package db

import (
	"context"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// MarkLoginExpired flags a session whose turn ended on Claude Code's login
// error (attention needs_you, reason model.LoginReason). It reports whether
// the row changed, so a mark already there isn't stamped (or pushed) again.
func (d *DB) MarkLoginExpired(ctx context.Context, sessionID string) (bool, error) {
	res, err := d.sql.ExecContext(ctx,
		`UPDATE sessions SET attention = ?, attention_reason = ?, attention_at = ?
		 WHERE session_id = ? AND NOT (COALESCE(attention,'') = ? AND COALESCE(attention_reason,'') = ?)`,
		model.AttentionNeedsYou, model.LoginReason, time.Now().Unix(),
		sessionID, model.AttentionNeedsYou, model.LoginReason)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// LoginExpiredIDs lists the sessions flagged by MarkLoginExpired that
// aren't archived, oldest mark first.
func (d *DB) LoginExpiredIDs(ctx context.Context) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT session_id FROM sessions WHERE attention = ? AND attention_reason = ? AND COALESCE(archived,0) = 0
		 ORDER BY attention_at`, model.AttentionNeedsYou, model.LoginReason)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ClearLoginMark drops a login mark (and only that), optionally leaving
// another reason in its place ("" = no attention).
func (d *DB) ClearLoginMark(ctx context.Context, sessionID, reason string) error {
	kind := ""
	if reason != "" {
		kind = model.AttentionNeedsYou
	}
	_, err := d.sql.ExecContext(ctx,
		`UPDATE sessions SET attention = ?, attention_reason = ?, attention_at = ?
		 WHERE session_id = ? AND attention = ? AND attention_reason = ?`,
		kind, reason, time.Now().Unix(), sessionID, model.AttentionNeedsYou, model.LoginReason)
	return err
}
