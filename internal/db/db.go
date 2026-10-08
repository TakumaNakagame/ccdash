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
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
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
		`ALTER TABLE sessions ADD COLUMN user_group TEXT`,
		`ALTER TABLE approvals ADD COLUMN tool_use_id TEXT`,
		`ALTER TABLE sessions ADD COLUMN account TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN num INTEGER`,
		`ALTER TABLE sessions ADD COLUMN gen_title TEXT`,
		`ALTER TABLE sessions ADD COLUMN gen_title_at INTEGER`,
		`ALTER TABLE sessions ADD COLUMN title_status TEXT`,
		`ALTER TABLE sessions ADD COLUMN attention TEXT`,
		`ALTER TABLE sessions ADD COLUMN attention_reason TEXT`,
		`ALTER TABLE sessions ADD COLUMN attention_at INTEGER`,
		`ALTER TABLE sessions ADD COLUMN color TEXT`,
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
	// Migrate the legacy `user_tab` column into `user_group`. The column
	// was renamed for terminology — "group" is the conceptual bucket; the
	// tab strip is the UI rendering. We copy values once, then keep the
	// dormant column around so a downgrade still finds its data. Idempotent
	// thanks to the IS NULL guard.
	if _, err := d.sql.Exec(`UPDATE sessions SET user_group = user_tab WHERE (user_group IS NULL OR user_group = '') AND user_tab IS NOT NULL AND user_tab <> ''`); err != nil {
		return fmt.Errorf("migrate user_tab → user_group: %w", err)
	}
	if err := d.backfillNums(); err != nil {
		return fmt.Errorf("backfill session numbers: %w", err)
	}
	if _, err := d.sql.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_num ON sessions(num)`); err != nil {
		return fmt.Errorf("migrate num index: %w", err)
	}
	return nil
}

// backfillNums gives every row without a num the next numbers in
// first-seen order, so pre-existing sessions get stable "#N" handles in the
// order they appeared. New rows get theirs at insert (upsertSession).
// Idempotent: only NULL rows are touched.
func (d *DB) backfillNums() error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var next int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(num), 0) FROM sessions`).Scan(&next); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT session_id FROM sessions WHERE num IS NULL ORDER BY first_seen, rowid`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		next++
		if _, err := tx.Exec(`UPDATE sessions SET num = ? WHERE session_id = ?`, next, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpsertSession inserts or merges a session row. last_seen only moves
// forward (MAX), so a late or stale writer can't rewind it.
func (d *DB) UpsertSession(ctx context.Context, s *model.Session) error {
	return d.upsertSession(ctx, s, lastSeenMax)
}

// UpsertSessionKeepLastSeen merges metadata without touching an existing
// row's last_seen (a new row still gets s.LastSeen, or now).
func (d *DB) UpsertSessionKeepLastSeen(ctx context.Context, s *model.Session) error {
	return d.upsertSession(ctx, s, lastSeenKeep)
}

// UpsertDiscoveredSession is UpsertSession for the discovery loop, whose
// last_seen (the transcript's last prompt time) is authoritative: it
// overwrites instead of taking the MAX, so a timestamp bumped by some
// earlier, looser rule converges back to the real prompt time.
func (d *DB) UpsertDiscoveredSession(ctx context.Context, s *model.Session) error {
	return d.upsertSession(ctx, s, lastSeenSet)
}

// lastSeenMode picks how an upsert treats an existing row's last_seen.
type lastSeenMode int

const (
	lastSeenMax  lastSeenMode = iota // only move forward
	lastSeenSet                      // overwrite
	lastSeenKeep                     // leave as is
)

func (d *DB) upsertSession(ctx context.Context, s *model.Session, mode lastSeenMode) error {
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
		                     first_seen, last_seen, status, account, num)
		VALUES (?, ?, ?, ?, ?,  ?, ?, ?,  ?, ?, ?, ?, ?,  ?, ?, ?, ?,
		        (SELECT COALESCE(MAX(num), 0) + 1 FROM sessions))
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
			last_seen = CASE ? WHEN 1 THEN excluded.last_seen WHEN 2 THEN sessions.last_seen ELSE MAX(excluded.last_seen, sessions.last_seen) END,
			status = excluded.status,
			account = COALESCE(NULLIF(excluded.account,''), sessions.account)
	`,
		s.SessionID, s.Cwd, s.Repo, s.Branch, s.Commit,
		s.WrapperPID, s.ProcPID, s.Pane,
		s.TmuxPane, s.TmuxSession, s.TranscriptPath, s.Model, s.Title,
		s.FirstSeen.Unix(), s.LastSeen.Unix(), string(s.Status), s.Account,
		int(mode),
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
		       COALESCE(s.title,''), COALESCE(s.custom_title,''), COALESCE(s.user_group,''),
		       COALESCE(s.archived,0), COALESCE(s.favorite,0),
		       COALESCE(s.summary,''), COALESCE(s.summary_status,''), COALESCE(s.summary_at,0),
		       s.first_seen, s.last_seen, s.status,
		       (SELECT COUNT(*) FROM approvals a WHERE a.session_id = s.session_id AND a.status = 'pending') AS pending,
		       COALESCE(s.account,''),
		       COALESCE(s.num,0), COALESCE(s.gen_title,''), COALESCE(s.gen_title_at,0), COALESCE(s.title_status,''),
		       COALESCE(s.attention,''), COALESCE(s.attention_reason,''), COALESCE(s.attention_at,0),
		       COALESCE(s.color,'')
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
		var first, last, sumAt, genAt, attAt int64
		var status string
		var arch, fav int
		if err := rows.Scan(&s.SessionID, &s.Cwd, &s.Repo, &s.Branch, &s.Commit,
			&s.WrapperPID, &s.ProcPID, &s.Pane,
			&s.TmuxPane, &s.TmuxSession,
			&s.TranscriptPath, &s.Model,
			&s.Title, &s.CustomTitle, &s.UserGroup,
			&arch, &fav,
			&s.Summary, &s.SummaryStatus, &sumAt,
			&first, &last, &status, &s.PendingCount, &s.Account,
			&s.Num, &s.GenTitle, &genAt, &s.TitleStatus,
			&s.Attention, &s.AttentionReason, &attAt, &s.ColorOverride); err != nil {
			return nil, err
		}
		finishAttention(&s, attAt)
		if genAt > 0 {
			s.GenTitleAt = time.Unix(genAt, 0).UTC()
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

// GetSession fetches a single session row by id. ok is false when no row
// matches — that's not an error condition, it's how callers like
// store.Local.Summarize and the server's transcript/summarize API handlers
// distinguish "no such session" (404/skip) from a hard DB failure.
func (d *DB) GetSession(ctx context.Context, sessionID string) (model.Session, bool, error) {
	row := d.sql.QueryRowContext(ctx, `
		SELECT s.session_id, s.cwd, COALESCE(s.repo,''), COALESCE(s.branch,''), COALESCE(s.commit_hash,''),
		       COALESCE(s.wrapper_pid,0), COALESCE(s.proc_pid,0), COALESCE(s.pane,''),
		       COALESCE(s.tmux_pane,''), COALESCE(s.tmux_session,''),
		       COALESCE(s.transcript_path,''), COALESCE(s.model,''),
		       COALESCE(s.title,''), COALESCE(s.custom_title,''), COALESCE(s.user_group,''),
		       COALESCE(s.archived,0), COALESCE(s.favorite,0),
		       COALESCE(s.summary,''), COALESCE(s.summary_status,''), COALESCE(s.summary_at,0),
		       s.first_seen, s.last_seen, s.status,
		       (SELECT COUNT(*) FROM approvals a WHERE a.session_id = s.session_id AND a.status = 'pending') AS pending,
		       COALESCE(s.account,''),
		       COALESCE(s.num,0), COALESCE(s.gen_title,''), COALESCE(s.gen_title_at,0), COALESCE(s.title_status,''),
		       COALESCE(s.attention,''), COALESCE(s.attention_reason,''), COALESCE(s.attention_at,0),
		       COALESCE(s.color,'')
		FROM sessions s
		WHERE s.session_id = ?
	`, sessionID)
	var s model.Session
	var first, last, sumAt, genAt, attAt int64
	var status string
	var arch, fav int
	err := row.Scan(&s.SessionID, &s.Cwd, &s.Repo, &s.Branch, &s.Commit,
		&s.WrapperPID, &s.ProcPID, &s.Pane,
		&s.TmuxPane, &s.TmuxSession,
		&s.TranscriptPath, &s.Model,
		&s.Title, &s.CustomTitle, &s.UserGroup,
		&arch, &fav,
		&s.Summary, &s.SummaryStatus, &sumAt,
		&first, &last, &status, &s.PendingCount, &s.Account,
		&s.Num, &s.GenTitle, &genAt, &s.TitleStatus,
		&s.Attention, &s.AttentionReason, &attAt, &s.ColorOverride)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Session{}, false, nil
	}
	if err != nil {
		return model.Session{}, false, err
	}
	s.FirstSeen = time.Unix(first, 0).UTC()
	s.LastSeen = time.Unix(last, 0).UTC()
	s.Status = model.SessionStatus(status)
	s.Archived = arch != 0
	s.Favorite = fav != 0
	if sumAt > 0 {
		s.SummaryAt = time.Unix(sumAt, 0).UTC()
	}
	if genAt > 0 {
		s.GenTitleAt = time.Unix(genAt, 0).UTC()
	}
	finishAttention(&s, attAt)
	return s, true, nil
}

// finishAttention completes the attention fields of a scanned row. A
// pending approval always means "needs you", whatever the stored column says
// (approvals live in their own table and resolve on their own).
func finishAttention(s *model.Session, attAt int64) {
	if attAt > 0 {
		s.AttentionAt = time.Unix(attAt, 0).UTC()
	}
	if s.PendingCount > 0 && s.Attention != model.AttentionNeedsYou {
		s.Attention = model.AttentionNeedsYou
		s.AttentionReason = "承認待ち"
	}
}

// SetAttention records what a session wants from the operator.
func (d *DB) SetAttention(ctx context.Context, sessionID, kind, reason string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE sessions SET attention = ?, attention_reason = ?, attention_at = ? WHERE session_id = ?`,
		kind, reason, time.Now().Unix(), sessionID)
	return err
}

// ClearAttention drops the attention mark; with only != "" it clears just
// that kind (a finished tool must not erase an unread "done").
func (d *DB) ClearAttention(ctx context.Context, sessionID, only string) error {
	q := `UPDATE sessions SET attention = '', attention_reason = '' WHERE session_id = ?`
	args := []any{sessionID}
	if only != "" {
		q += ` AND attention = ?`
		args = append(args, only)
	}
	_, err := d.sql.ExecContext(ctx, q, args...)
	return err
}

// MarkSeen records that the operator looked at a session: an unread
// "done" becomes read.
func (d *DB) MarkSeen(ctx context.Context, sessionID string) error {
	return d.ClearAttention(ctx, sessionID, model.AttentionDone)
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

// SetColor stores the operator's color for a session (a "#rrggbb"), or
// clears it with "" so Session.Color falls back to the hashed one.
func (d *DB) SetColor(ctx context.Context, sessionID, color string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE sessions SET color = NULLIF(?, '') WHERE session_id = ?`, color, sessionID)
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

// GetSetting returns the raw string value for a key; missing keys come back
// as ("", nil). Other errors propagate.
func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting upserts a setting value. Pass an empty string to keep the row
// but mark it explicitly empty; deletion is intentional and goes through
// DeleteSetting.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, key, value, time.Now().UTC().Unix())
	return err
}

// AllSettings returns every row of the settings table as a key→value map.
// Missing keys are simply absent (callers treat that the same as ""). This
// is the bulk-read primitive behind settings.Load and GET /api/settings —
// one query instead of one round trip per key.
func (d *DB) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetUserGroup assigns a session to an operator-named group (rendered as
// a tab in the strip). Empty clears the assignment so the session falls
// back to its repo-based grouping.
func (d *DB) SetUserGroup(ctx context.Context, sessionID, group string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE sessions SET user_group = ? WHERE session_id = ?`, group, sessionID)
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

// SweepRunningSummaries flips any summary_status='running' row to 'error'.
// Run on collector startup: the summarize goroutine lives in the collector
// process, so a 'running' row at boot means a previous run died mid-flight
// — without the sweep the list row would show a spinner forever.
func (d *DB) SweepRunningSummaries(ctx context.Context) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE sessions
		SET summary = 'summarize interrupted (collector restarted)', summary_status = 'error', summary_at = ?
		WHERE summary_status = 'running'
	`, time.Now().UTC().Unix())
	return err
}

// SetTitleStatus marks title generation as running / error for the given
// sessions without touching gen_title, so the rows can show progress.
func (d *DB) SetTitleStatus(ctx context.Context, sessionIDs []string, status string) error {
	for _, id := range sessionIDs {
		if _, err := d.sql.ExecContext(ctx, `UPDATE sessions SET title_status = ? WHERE session_id = ?`, status, id); err != nil {
			return err
		}
	}
	return nil
}

// SetGenTitle stores a generated title and marks generation done.
func (d *DB) SetGenTitle(ctx context.Context, sessionID, title string) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE sessions SET gen_title = ?, gen_title_at = ?, title_status = 'done'
		WHERE session_id = ?
	`, title, time.Now().UTC().Unix(), sessionID)
	return err
}

// SweepRunningTitles is SweepRunningSummaries for title generation: a
// 'running' row at collector startup belongs to a run that died with the
// previous process.
func (d *DB) SweepRunningTitles(ctx context.Context) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE sessions SET title_status = 'error' WHERE title_status = 'running'`)
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
