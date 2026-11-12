package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Ignored sessions are automated claude runs (a cron job's `claude -p`,
// `claude mcp list` …) that ccdash must not show anywhere. Their IDs are
// remembered in ignored_sessions so discovery, which only sees the
// transcript, never brings them back; their rows, events and approvals
// are dropped.

// keepSQL is true for a row the operator has done something with: such a
// row is never forgotten, whatever marks it automated.
const keepSQL = `(favorite = 1 OR COALESCE(project, '') <> '' OR COALESCE(custom_title, '') <> ''
	OR later_at IS NOT NULL OR grid_at IS NOT NULL
	OR session_id IN (SELECT session_id FROM task_sessions))`

// IgnoreSession marks id as an automated run and removes what ccdash
// recorded for it. It reports false (and changes nothing) when the row is
// one the operator kept (pinned, in a project, renamed, watch later, on
// the grid, linked to a task) — that is a real session.
func (d *DB) IgnoreSession(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	var keep bool
	err := d.sql.QueryRowContext(ctx, `SELECT `+keepSQL+` FROM sessions WHERE session_id = ?`, id).Scan(&keep)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if keep {
		return false, nil
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`INSERT OR IGNORE INTO ignored_sessions (session_id, at) VALUES (?, ?)`,
		`DELETE FROM approvals WHERE session_id = ?`,
		`DELETE FROM events WHERE session_id = ?`,
		`DELETE FROM sessions WHERE session_id = ?`,
	} {
		args := []any{id}
		if q[0] == 'I' {
			args = append(args, time.Now().UnixMilli())
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return false, fmt.Errorf("ignore session: %w", err)
		}
	}
	return true, tx.Commit()
}

// IsIgnored reports whether id was marked by IgnoreSession.
func (d *DB) IsIgnored(ctx context.Context, id string) bool {
	if id == "" {
		return false
	}
	var one int
	err := d.sql.QueryRowContext(ctx, `SELECT 1 FROM ignored_sessions WHERE session_id = ?`, id).Scan(&one)
	return err == nil
}

// IgnoredIDs is the whole ignored set (discovery reads it once per tick).
func (d *DB) IgnoredIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT session_id FROM ignored_sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SessionExists reports whether a row for id exists.
func (d *DB) SessionExists(ctx context.Context, id string) bool {
	var one int
	return d.sql.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE session_id = ?`, id).Scan(&one) == nil
}

// AutomatedRow is a stored session that looks like a leftover automated
// run (PruneCandidates).
type AutomatedRow struct {
	SessionID  string
	Num        int
	Cwd        string
	Title      string
	Status     string
	Entrypoint string
	FirstSeen  time.Time
	Why        string // "no transcript" / "ignored folder"
}

// PruneCandidates lists stored rows that are automated runs: headless
// (sdk-*) rows with no prompt (empty title) whose transcript file doesn't
// exist (`claude mcp list` and friends: only a SessionEnd ever arrived —
// a row with a prompt but a missing transcript may be a real session
// moved from another machine, so it stays), and headless rows whose cwd
// the inDir func accepts (the ignore_dirs setting). Rows the operator kept
// (keepSQL), running ones and ones started by ccdash (origin) are left
// out. exists reports whether a transcript path is on disk.
func (d *DB) PruneCandidates(ctx context.Context, inDir func(cwd string) bool, exists func(path string) bool) ([]AutomatedRow, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT session_id, COALESCE(num, 0), cwd, COALESCE(title, ''), status,
		COALESCE(entrypoint, ''), first_seen, COALESCE(transcript_path, '')
		FROM sessions
		WHERE COALESCE(entrypoint, '') LIKE 'sdk-%' AND status NOT IN ('active', 'idle')
		  AND COALESCE(origin, '') = '' AND NOT `+keepSQL+`
		ORDER BY num`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AutomatedRow
	for rows.Next() {
		var r AutomatedRow
		var first int64
		var tp string
		if err := rows.Scan(&r.SessionID, &r.Num, &r.Cwd, &r.Title, &r.Status, &r.Entrypoint, &first, &tp); err != nil {
			return nil, err
		}
		r.FirstSeen = unixAny(first)
		switch {
		case inDir != nil && inDir(r.Cwd):
			r.Why = "ignored folder"
		case r.Title == "" && (tp == "" || !exists(tp)):
			r.Why = "no transcript"
		default:
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// unixAny reads a timestamp stored as Unix seconds or milliseconds.
func unixAny(v int64) time.Time {
	if v > 1e12 {
		return time.UnixMilli(v)
	}
	return time.Unix(v, 0)
}
