package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/takumanakagame/ccmanage/internal/model"
)

type DB struct {
	sql *sql.DB
}

func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)", path)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := sqldb.Ping(); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	d := &DB{sql: sqldb}
	if err := d.migrate(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() error { return d.sql.Close() }

func (d *DB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			session_id TEXT PRIMARY KEY,
			cwd TEXT NOT NULL,
			repo TEXT,
			branch TEXT,
			commit_hash TEXT,
			wrapper_pid INTEGER,
			proc_pid INTEGER,
			pane TEXT,
			tmux_pane TEXT,
			tmux_session TEXT,
			transcript_path TEXT,
			model TEXT,
			title TEXT,
			custom_title TEXT,
			archived INTEGER NOT NULL DEFAULT 0,
			favorite INTEGER NOT NULL DEFAULT 0,
			first_seen INTEGER NOT NULL,
			last_seen INTEGER NOT NULL,
			status TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			ts INTEGER NOT NULL,
			event_type TEXT NOT NULL,
			tool TEXT,
			summary TEXT,
			payload TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_session_ts ON events(session_id, ts)`,
		`CREATE TABLE IF NOT EXISTS approvals (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			ts INTEGER NOT NULL,
			tool TEXT NOT NULL,
			tool_use_id TEXT,
			tool_input TEXT NOT NULL,
			status TEXT NOT NULL,
			reason TEXT,
			decided_at INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_approvals_session ON approvals(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_approvals_status ON approvals(status)`,
	}
	for _, s := range stmts {
		if _, err := d.sql.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// Add columns to existing databases. ALTER TABLE ADD COLUMN errors when
	// the column already exists; we ignore those since the create-table above
	// already covers fresh installs.
	for _, alter := range []string{
		`ALTER TABLE sessions ADD COLUMN proc_pid INTEGER`,
		`ALTER TABLE sessions ADD COLUMN pane TEXT`,
		`ALTER TABLE sessions ADD COLUMN title TEXT`,
		`ALTER TABLE sessions ADD COLUMN custom_title TEXT`,
		`ALTER TABLE sessions ADD COLUMN archived INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN favorite INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN summary TEXT`,
		`ALTER TABLE sessions ADD COLUMN summary_status TEXT`,
		`ALTER TABLE sessions ADD COLUMN summary_at INTEGER`,
		`ALTER TABLE sessions ADD COLUMN user_tab TEXT`,
		`ALTER TABLE approvals ADD COLUMN tool_use_id TEXT`,
	} {
		if _, err := d.sql.Exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migrate alter: %w", err)
		}
	}
	if _, err := d.sql.Exec(`CREATE INDEX IF NOT EXISTS idx_approvals_tool_use ON approvals(tool_use_id) WHERE tool_use_id IS NOT NULL`); err != nil {
		// Partial index syntax may be unsupported; ignore.
	}
	// One-shot cleanup: earlier builds let summary-spawned `claude -p`
	// invocations register as full sessions (because they inherited our
	// own hooks). Wipe any rows that match that pattern so the dashboard
	// list is clean after upgrade.
	if _, err := d.sql.Exec(`DELETE FROM sessions WHERE title LIKE '[ccdash:summary]%' OR custom_title LIKE '[ccdash:summary]%'`); err != nil {
		return fmt.Errorf("cleanup ccdash:summary sessions: %w", err)
	}
	return nil
}

func (d *DB) UpsertSession(ctx context.Context, s *model.Session) error {
	now := time.Now().UTC()
	if s.FirstSeen.IsZero() {
		s.FirstSeen = now
	}
	if s.LastSeen.IsZero() {
		s.LastSeen = now
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO sessions (session_id, cwd, repo, branch, commit_hash,
		                     wrapper_pid, proc_pid, pane,
		                     tmux_pane, tmux_session, transcript_path, model, title,
		                     first_seen, last_seen, status)
		VALUES (?, ?, ?, ?, ?,  ?, ?, ?,  ?, ?, ?, ?, ?,  ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			cwd = COALESCE(NULLIF(excluded.cwd,''), sessions.cwd),
			repo = COALESCE(NULLIF(excluded.repo,''), sessions.repo),
			branch = COALESCE(NULLIF(excluded.branch,''), sessions.branch),
			commit_hash = COALESCE(NULLIF(excluded.commit_hash,''), sessions.commit_hash),
			wrapper_pid = COALESCE(NULLIF(excluded.wrapper_pid,0), sessions.wrapper_pid),
			proc_pid = excluded.proc_pid,
			pane = excluded.pane,
			tmux_pane = COALESCE(NULLIF(excluded.tmux_pane,''), sessions.tmux_pane),
			tmux_session = COALESCE(NULLIF(excluded.tmux_session,''), sessions.tmux_session),
			transcript_path = COALESCE(NULLIF(excluded.transcript_path,''), sessions.transcript_path),
			model = COALESCE(NULLIF(excluded.model,''), sessions.model),
			title = COALESCE(NULLIF(excluded.title,''), sessions.title),
			last_seen = MAX(excluded.last_seen, sessions.last_seen),
			status = excluded.status
	`,
		s.SessionID, s.Cwd, s.Repo, s.Branch, s.Commit,
		s.WrapperPID, s.ProcPID, s.Pane,
		s.TmuxPane, s.TmuxSession, s.TranscriptPath, s.Model, s.Title,
		s.FirstSeen.Unix(), s.LastSeen.Unix(), string(s.Status),
	)
	return err
}

func (d *DB) TouchSession(ctx context.Context, sessionID string, status model.SessionStatus) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE sessions SET last_seen = ?, status = ? WHERE session_id = ?`,
		time.Now().UTC().Unix(), string(status), sessionID)
	return err
}

func (d *DB) AppendEvent(ctx context.Context, e *model.Event) (int64, error) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage("{}")
	}
	res, err := d.sql.ExecContext(ctx, `
		INSERT INTO events (session_id, ts, event_type, tool, summary, payload)
		VALUES (?, ?, ?, ?, ?, ?)
	`, e.SessionID, e.Timestamp.Unix(), string(e.EventType), e.Tool, e.Summary, string(e.Payload))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) InsertApproval(ctx context.Context, a *model.Approval) (int64, error) {
	if a.Timestamp.IsZero() {
		a.Timestamp = time.Now().UTC()
	}
	if len(a.ToolInput) == 0 {
		a.ToolInput = json.RawMessage("{}")
	}
	res, err := d.sql.ExecContext(ctx, `
		INSERT INTO approvals (session_id, ts, tool, tool_use_id, tool_input, status, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, a.SessionID, a.Timestamp.Unix(), a.Tool, a.ToolUseID, string(a.ToolInput), string(a.Status), a.Reason)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) UpdateApprovalStatus(ctx context.Context, id int64, status model.ApprovalStatus, reason string) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE approvals SET status = ?, reason = ?, decided_at = ? WHERE id = ?
	`, string(status), reason, time.Now().UTC().Unix(), id)
	return err
}

// ResolvePendingByToolUseID closes any pending approval whose tool_use_id
// matches. PermissionRequest hooks don't carry a tool_use_id today, so this
// is best-effort; ResolveOldestPendingForTool is the fallback that actually
// fires for most cases.
func (d *DB) ResolvePendingByToolUseID(ctx context.Context, sessionID, toolUseID string, status model.ApprovalStatus) error {
	if toolUseID == "" {
		return nil
	}
	_, err := d.sql.ExecContext(ctx, `
		UPDATE approvals
		SET status = ?, decided_at = ?
		WHERE session_id = ? AND tool_use_id = ? AND status = 'pending'
	`, string(status), time.Now().UTC().Unix(), sessionID, toolUseID)
	return err
}

// ResolveOldestPendingForTool closes the single oldest pending approval that
// matches session_id + tool name. We call this from PostToolUse handlers as a
// fallback because PermissionRequest payloads don't include tool_use_id —
// matching by tool name is good enough in practice since approvals are
// processed FIFO by Claude.
func (d *DB) ResolveOldestPendingForTool(ctx context.Context, sessionID, tool string, status model.ApprovalStatus) error {
	if sessionID == "" || tool == "" {
		return nil
	}
	_, err := d.sql.ExecContext(ctx, `
		UPDATE approvals
		SET status = ?, decided_at = ?
		WHERE id = (
			SELECT id FROM approvals
			WHERE session_id = ? AND tool = ? AND status = 'pending'
			ORDER BY ts ASC LIMIT 1
		)
	`, string(status), time.Now().UTC().Unix(), sessionID, tool)
	return err
}

// MarkStalePendingTimeout flips pending approvals older than `age` to
// 'timeout'. We run this from the server's discovery loop so the dashboard
// doesn't accumulate phantom approvals when hooks land out of order or when
// Claude's own 30-second hook timeout has already elapsed.
func (d *DB) MarkStalePendingTimeout(ctx context.Context, age time.Duration) error {
	cutoff := time.Now().UTC().Add(-age).Unix()
	_, err := d.sql.ExecContext(ctx, `
		UPDATE approvals
		SET status = 'timeout', decided_at = ?
		WHERE status = 'pending' AND ts < ?
	`, time.Now().UTC().Unix(), cutoff)
	return err
}

// ListSessions returns sessions matching the archived flag. When archived is
// false you get the working set (favorites first, then by last_seen DESC);
// when true you get the archive view ordered the same way.
func (d *DB) ListSessions(ctx context.Context, archived bool) ([]model.Session, error) {
	archivedInt := 0
	if archived {
		archivedInt = 1
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT s.session_id, s.cwd, COALESCE(s.repo,''), COALESCE(s.branch,''), COALESCE(s.commit_hash,''),
		       COALESCE(s.wrapper_pid,0), COALESCE(s.proc_pid,0), COALESCE(s.pane,''),
		       COALESCE(s.tmux_pane,''), COALESCE(s.tmux_session,''),
		       COALESCE(s.transcript_path,''), COALESCE(s.model,''),
		       COALESCE(s.title,''), COALESCE(s.custom_title,''), COALESCE(s.user_tab,''),
		       COALESCE(s.archived,0), COALESCE(s.favorite,0),
		       COALESCE(s.summary,''), COALESCE(s.summary_status,''), COALESCE(s.summary_at,0),
		       s.first_seen, s.last_seen, s.status,
		       (SELECT COUNT(*) FROM approvals a WHERE a.session_id = s.session_id AND a.status = 'pending') AS pending
		FROM sessions s
		WHERE COALESCE(s.archived,0) = ?
		ORDER BY COALESCE(s.favorite,0) DESC, s.last_seen DESC
	`, archivedInt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Session
	for rows.Next() {
		var s model.Session
		var first, last, sumAt int64
		var status string
		var arch, fav int
		if err := rows.Scan(&s.SessionID, &s.Cwd, &s.Repo, &s.Branch, &s.Commit,
			&s.WrapperPID, &s.ProcPID, &s.Pane,
			&s.TmuxPane, &s.TmuxSession,
			&s.TranscriptPath, &s.Model,
			&s.Title, &s.CustomTitle, &s.UserTab,
			&arch, &fav,
			&s.Summary, &s.SummaryStatus, &sumAt,
			&first, &last, &status, &s.PendingCount); err != nil {
			return nil, err
		}
		s.FirstSeen = time.Unix(first, 0).UTC()
		s.LastSeen = time.Unix(last, 0).UTC()
		s.Status = model.SessionStatus(status)
		s.Archived = arch != 0
		s.Favorite = fav != 0
		if sumAt > 0 {
			s.SummaryAt = time.Unix(sumAt, 0).UTC()
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetArchived flips the archived flag.
func (d *DB) SetArchived(ctx context.Context, sessionID string, archived bool) error {
	v := 0
	if archived {
		v = 1
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE sessions SET archived = ? WHERE session_id = ?`, v, sessionID)
	return err
}

// SetFavorite flips the favorite flag.
func (d *DB) SetFavorite(ctx context.Context, sessionID string, favorite bool) error {
	v := 0
	if favorite {
		v = 1
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE sessions SET favorite = ? WHERE session_id = ?`, v, sessionID)
	return err
}

// SetCustomTitle stores an operator-supplied title that overrides the
// transcript-derived one in the UI. Pass an empty string to clear.
func (d *DB) SetCustomTitle(ctx context.Context, sessionID, title string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE sessions SET custom_title = ? WHERE session_id = ?`, title, sessionID)
	return err
}

// SetUserTab assigns a session to an operator-named tab. The tab key is
// just a string — empty clears the assignment so the session falls back to
// its repo-based group.
func (d *DB) SetUserTab(ctx context.Context, sessionID, tab string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE sessions SET user_tab = ? WHERE session_id = ?`, tab, sessionID)
	return err
}

// SetSummaryStatus marks a summary as in-progress / done / error without
// touching the cached summary text. Used to surface "summarizing..." in
// the list row while the background goroutine runs.
func (d *DB) SetSummaryStatus(ctx context.Context, sessionID, status string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE sessions SET summary_status = ? WHERE session_id = ?`, status, sessionID)
	return err
}

// SetSummary writes the summary text and updates status / timestamp in one
// shot. Pass status "done" or "error" depending on the outcome.
func (d *DB) SetSummary(ctx context.Context, sessionID, summary, status string) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE sessions SET summary = ?, summary_status = ?, summary_at = ?
		WHERE session_id = ?
	`, summary, status, time.Now().UTC().Unix(), sessionID)
	return err
}

func (d *DB) ListEvents(ctx context.Context, sessionID string, limit int) ([]model.Event, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, session_id, ts, event_type, COALESCE(tool,''), COALESCE(summary,''), payload
		FROM events
		WHERE session_id = ?
		ORDER BY id DESC
		LIMIT ?
	`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		var e model.Event
		var ts int64
		var et string
		var payload string
		if err := rows.Scan(&e.ID, &e.SessionID, &ts, &et, &e.Tool, &e.Summary, &payload); err != nil {
			return nil, err
		}
		e.Timestamp = time.Unix(ts, 0).UTC()
		e.EventType = model.EventType(et)
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	// reverse so caller gets ascending order
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func (d *DB) ListPendingApprovals(ctx context.Context) ([]model.Approval, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, session_id, ts, tool, COALESCE(tool_use_id,''), tool_input, status, COALESCE(reason,'')
		FROM approvals
		WHERE status = 'pending'
		ORDER BY ts ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Approval
	for rows.Next() {
		var a model.Approval
		var ts int64
		var status, input string
		if err := rows.Scan(&a.ID, &a.SessionID, &ts, &a.Tool, &a.ToolUseID, &input, &status, &a.Reason); err != nil {
			return nil, err
		}
		a.Timestamp = time.Unix(ts, 0).UTC()
		a.Status = model.ApprovalStatus(status)
		a.ToolInput = json.RawMessage(input)
		out = append(out, a)
	}
	return out, rows.Err()
}
