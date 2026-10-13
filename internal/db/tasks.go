package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/tasks"
)

// The to-do list (docs/decisions/0006-tasks.md). The rules live in
// internal/tasks; this file loads tasks, hands them to those functions and
// saves what changed, one transaction per operation, bumping task_meta's
// rev so pollers notice.

// ErrTaskNotFound is returned for an unknown id or key.
var ErrTaskNotFound = errors.New("task not found")

func (d *DB) migrateTasks() error {
	for _, s := range []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			key TEXT,
			title TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 3,
			status TEXT NOT NULL DEFAULT 'todo',
			plan TEXT NOT NULL DEFAULT '',
			plan_by TEXT NOT NULL DEFAULT '',
			plan_on TEXT NOT NULL DEFAULT '',
			due TEXT NOT NULL DEFAULT '',
			project TEXT NOT NULL DEFAULT '',
			note TEXT NOT NULL DEFAULT '',
			links TEXT NOT NULL DEFAULT '[]',
			source TEXT NOT NULL DEFAULT '',
			created_on TEXT NOT NULL DEFAULT '',
			done_on TEXT NOT NULL DEFAULT '',
			ord REAL,
			archived_at INTEGER,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_key ON tasks(key) WHERE key IS NOT NULL AND key <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_project ON tasks(project)`,
		`CREATE TABLE IF NOT EXISTS task_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id INTEGER NOT NULL,
			at TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT '',
			text TEXT NOT NULL,
			link TEXT NOT NULL DEFAULT '',
			ref TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_task_events_ref ON task_events(task_id, ref)`,
		`CREATE TABLE IF NOT EXISTS task_sessions (
			task_id INTEGER NOT NULL,
			session_id TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (task_id, session_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_task_sessions_session ON task_sessions(session_id)`,
		`CREATE TABLE IF NOT EXISTS task_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	} {
		if _, err := d.sql.Exec(s); err != nil {
			return fmt.Errorf("migrate tasks: %w", err)
		}
	}
	return nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func bumpTasksRev(ctx context.Context, q execer) error {
	_, err := q.ExecContext(ctx, `INSERT INTO task_meta (key, value) VALUES ('rev', '1')
		ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)`)
	return err
}

func taskMeta(ctx context.Context, q execer, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM task_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func setTaskMeta(ctx context.Context, q execer, key, value string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO task_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// TasksRev is the change counter: it goes up with every task change.
func (d *DB) TasksRev(ctx context.Context) (int64, error) {
	v, err := taskMeta(ctx, d.sql, "rev")
	if err != nil || v == "" {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

const taskCols = `id, COALESCE(key,''), title, priority, status, plan, plan_by, plan_on, due, project, note,
	links, source, created_on, done_on, ord, archived_at, updated_at`

func loadTasks(ctx context.Context, q execer, where string, args ...any) ([]model.Task, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	var out []model.Task
	idx := map[int64]int{}
	for rows.Next() {
		var t model.Task
		var links string
		var ord sql.NullFloat64
		var archived sql.NullInt64
		var updated int64
		if err := rows.Scan(&t.ID, &t.Key, &t.Title, &t.Priority, &t.Status, &t.Plan, &t.PlanBy, &t.PlanOn,
			&t.Due, &t.Project, &t.Note, &links, &t.Source, &t.CreatedOn, &t.DoneOn, &ord, &archived, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(links), &t.Links); err != nil || t.Links == nil {
			t.Links = []string{}
		}
		if ord.Valid {
			v := ord.Float64
			t.Order = &v
		}
		t.Archived = archived.Valid
		t.UpdatedAt = time.UnixMilli(updated).UTC()
		t.Timeline = []model.TaskNote{}
		idx[t.ID] = len(out)
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	// Timelines and session links: one query each, filtered in Go (the
	// list is small; this keeps the SQL simple for single-task loads too).
	ev, err := q.QueryContext(ctx, `SELECT task_id, at, source, text, link, ref FROM task_events ORDER BY at, id`)
	if err != nil {
		return nil, err
	}
	for ev.Next() {
		var id int64
		var n model.TaskNote
		if err := ev.Scan(&id, &n.At, &n.Source, &n.Text, &n.Link, &n.Ref); err != nil {
			ev.Close()
			return nil, err
		}
		if i, ok := idx[id]; ok {
			out[i].Timeline = append(out[i].Timeline, n)
		}
	}
	ev.Close()
	ss, err := q.QueryContext(ctx, `SELECT task_id, session_id FROM task_sessions ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer ss.Close()
	for ss.Next() {
		var id int64
		var sid string
		if err := ss.Scan(&id, &sid); err != nil {
			return nil, err
		}
		if i, ok := idx[id]; ok {
			out[i].Sessions = append(out[i].Sessions, sid)
		}
	}
	return out, ss.Err()
}

// ListTasks returns the list, archived tasks only when asked.
func (d *DB) ListTasks(ctx context.Context, includeArchived bool) ([]model.Task, error) {
	where := `WHERE archived_at IS NULL`
	if includeArchived {
		where = ``
	}
	return loadTasks(ctx, d.sql, where)
}

// taskWhere turns "11", "#11" or a key into a WHERE clause.
func taskWhere(ident string) (string, []any) {
	ident = strings.TrimSpace(ident)
	if n, err := strconv.ParseInt(strings.TrimPrefix(ident, "#"), 10, 64); err == nil {
		return `WHERE id = ?`, []any{n}
	}
	return `WHERE key = ?`, []any{ident}
}

func loadOne(ctx context.Context, q execer, ident string) (model.Task, error) {
	where, args := taskWhere(ident)
	ts, err := loadTasks(ctx, q, where, args...)
	if err != nil {
		return model.Task{}, err
	}
	if len(ts) == 0 {
		return model.Task{}, fmt.Errorf("%w: %s", ErrTaskNotFound, ident)
	}
	return ts[0], nil
}

// GetTask finds a task by id ("11" / "#11") or key, archived or not.
func (d *DB) GetTask(ctx context.Context, ident string) (model.Task, error) {
	return loadOne(ctx, d.sql, ident)
}

// saveTask writes t (inserting when ID is 0 and filling it in, upserting
// by id otherwise), adds its timeline entries that aren't stored yet and
// replaces its session links.
func saveTask(ctx context.Context, q execer, t *model.Task, now time.Time) error {
	links, _ := json.Marshal(t.Links)
	if t.Links == nil {
		links = []byte("[]")
	}
	var ord, archived, key any
	if t.Order != nil {
		ord = *t.Order
	}
	if t.Archived {
		archived = now.UnixMilli()
	}
	if t.Key != "" {
		key = t.Key
	}
	cols := []any{key, t.Title, t.Priority, t.Status, t.Plan, t.PlanBy, t.PlanOn, t.Due, t.Project, t.Note,
		string(links), t.Source, t.CreatedOn, t.DoneOn, ord, archived, now.UnixMilli()}
	if t.ID == 0 {
		res, err := q.ExecContext(ctx, `INSERT INTO tasks (key, title, priority, status, plan, plan_by, plan_on, due,
			project, note, links, source, created_on, done_on, ord, archived_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, cols...)
		if err != nil {
			return taskKeyErr(err, t.Key)
		}
		if t.ID, err = res.LastInsertId(); err != nil {
			return err
		}
	} else {
		// Upsert by id: import inserts with the file's ids; edits update.
		// archived_at keeps its first stamp while the task stays archived.
		_, err := q.ExecContext(ctx, `INSERT INTO tasks (id, key, title, priority, status, plan, plan_by, plan_on, due,
			project, note, links, source, created_on, done_on, ord, archived_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET key=excluded.key, title=excluded.title, priority=excluded.priority,
				status=excluded.status, plan=excluded.plan, plan_by=excluded.plan_by, plan_on=excluded.plan_on,
				due=excluded.due, project=excluded.project, note=excluded.note, links=excluded.links,
				source=excluded.source, created_on=excluded.created_on, done_on=excluded.done_on, ord=excluded.ord,
				archived_at=CASE WHEN excluded.archived_at IS NULL THEN NULL ELSE COALESCE(tasks.archived_at, excluded.archived_at) END,
				updated_at=excluded.updated_at`, append([]any{t.ID}, cols...)...)
		if err != nil {
			return taskKeyErr(err, t.Key)
		}
	}
	for _, n := range t.Timeline {
		if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO task_events (task_id, at, source, text, link, ref)
			VALUES (?,?,?,?,?,?)`, t.ID, n.At, n.Source, n.Text, n.Link, n.Ref); err != nil {
			return err
		}
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM task_sessions WHERE task_id = ?`, t.ID); err != nil {
		return err
	}
	for i, sid := range t.Sessions {
		if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO task_sessions (task_id, session_id, created_at) VALUES (?,?,?)`,
			t.ID, sid, now.UnixMilli()+int64(i)); err != nil {
			return err
		}
	}
	return nil
}

func taskKeyErr(err error, key string) error {
	if strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("another task already has key %q", key)
	}
	return err
}

// inTx runs fn in a transaction and bumps the rev when it reports a change.
func (d *DB) inTx(ctx context.Context, fn func(tx *sql.Tx) (bool, error)) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed, err := fn(tx)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := bumpTasksRev(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ensureProjects creates the projects tasks point at. It runs after the
// task transaction commits: ensureProject writes outside that transaction,
// and SQLite would make it wait on the open one.
func (d *DB) ensureProjects(ctx context.Context, names ...string) error {
	for _, n := range names {
		if n == "" {
			continue
		}
		if err := d.ensureProject(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

// AddTask adds a task and returns it with its id.
func (d *DB) AddTask(ctx context.Context, in tasks.Input) (model.Task, error) {
	now := time.Now()
	t, err := tasks.New(in, now)
	if err != nil {
		return t, err
	}
	err = d.inTx(ctx, func(tx *sql.Tx) (bool, error) {
		return true, saveTask(ctx, tx, &t, now)
	})
	if err != nil {
		return t, err
	}
	if err := d.ensureProjects(ctx, t.Project); err != nil {
		return t, err
	}
	return d.GetTask(ctx, strconv.FormatInt(t.ID, 10))
}

// UpdateTask applies a patch to the task ident (id or key).
func (d *DB) UpdateTask(ctx context.Context, ident string, p tasks.Patch) (model.Task, error) {
	now := time.Now()
	var t model.Task
	err := d.inTx(ctx, func(tx *sql.Tx) (bool, error) {
		var err error
		if t, err = loadOne(ctx, tx, ident); err != nil {
			return false, err
		}
		changed, err := tasks.Apply(&t, p, now)
		if err != nil || !changed {
			return false, err
		}
		return true, saveTask(ctx, tx, &t, now)
	})
	if err != nil {
		return t, err
	}
	if p.Project != nil {
		if err := d.ensureProjects(ctx, t.Project); err != nil {
			return t, err
		}
	}
	return d.GetTask(ctx, strconv.FormatInt(t.ID, 10))
}

// AddTaskNote appends a timeline entry (skipped when its ref is there
// already). At defaults to now.
func (d *DB) AddTaskNote(ctx context.Context, ident string, n model.TaskNote) (model.Task, error) {
	now := time.Now()
	if n.At == "" {
		n.At = tasks.Stamp(now)
	}
	var t model.Task
	err := d.inTx(ctx, func(tx *sql.Tx) (bool, error) {
		var err error
		if t, err = loadOne(ctx, tx, ident); err != nil {
			return false, err
		}
		if !tasks.AddNote(&t, n) {
			return false, nil
		}
		return true, saveTask(ctx, tx, &t, now)
	})
	if err != nil {
		return t, err
	}
	return d.GetTask(ctx, strconv.FormatInt(t.ID, 10))
}

// OrderTasks places the given tasks first, in that order, ahead of the
// tasks never placed by hand. The UI passes one bucket's ids.
func (d *DB) OrderTasks(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return d.inTx(ctx, func(tx *sql.Tx) (bool, error) {
		for i, id := range ids {
			res, err := tx.ExecContext(ctx, `UPDATE tasks SET ord = ?, updated_at = ? WHERE id = ?`, float64(i+1), time.Now().UnixMilli(), id)
			if err != nil {
				return false, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return false, fmt.Errorf("%w: #%d", ErrTaskNotFound, id)
			}
		}
		return true, nil
	})
}

// SyncTasks merges an automated run's proposal (tasks.Merge) and records
// its synced_at as the next run's starting point.
func (d *DB) SyncTasks(ctx context.Context, p tasks.Proposal) (tasks.SyncResult, error) {
	now := time.Now()
	projects, err := d.ListProjects(ctx)
	if err != nil {
		return tasks.SyncResult{}, err
	}
	known := map[string]bool{}
	for _, pr := range projects {
		known[pr.Name] = true
	}
	var res tasks.SyncResult
	err = d.inTx(ctx, func(tx *sql.Tx) (bool, error) {
		all, err := loadTasks(ctx, tx, ``)
		if err != nil {
			return false, err
		}
		ptrs := make([]*model.Task, len(all))
		for i := range all {
			ptrs[i] = &all[i]
		}
		var changed []*model.Task
		changed, res = tasks.Merge(ptrs, p, func(n string) bool { return known[n] }, now)
		for _, t := range changed {
			if err := saveTask(ctx, tx, t, now); err != nil {
				return false, err
			}
		}
		if p.SyncedAt != "" {
			if err := setTaskMeta(ctx, tx, "last_sync", p.SyncedAt); err != nil {
				return false, err
			}
		}
		// A dry run rolls back: nothing saved, synced_at not moved.
		return !p.DryRun && (len(changed) > 0 || p.SyncedAt != ""), nil
	})
	res.DryRun = p.DryRun
	return res, err
}

// LastTaskSync is the previous automated run's synced_at.
func (d *DB) LastTaskSync(ctx context.Context) (string, error) {
	return taskMeta(ctx, d.sql, "last_sync")
}

// ImportTasks brings in a work-status tasks.json, keeping its ids. It
// refuses when the list isn't empty. With dryRun it only reports the plan.
func (d *DB) ImportTasks(ctx context.Context, f tasks.LegacyFile, opt tasks.ImportOptions, dryRun bool) (tasks.ImportPlan, error) {
	now := time.Now()
	projects, err := d.ListProjects(ctx)
	if err != nil {
		return tasks.ImportPlan{}, err
	}
	known := map[string]bool{}
	for _, pr := range projects {
		known[pr.Name] = true
	}
	plan, err := tasks.PlanImport(f, opt, func(n string) bool { return known[n] }, now)
	if err != nil || dryRun {
		return plan, err
	}
	err = d.inTx(ctx, func(tx *sql.Tx) (bool, error) {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return false, fmt.Errorf("the task list already has %d tasks; import only into an empty list", n)
		}
		for i := range plan.Converted {
			if err := saveTask(ctx, tx, &plan.Converted[i], now); err != nil {
				return false, fmt.Errorf("#%d: %w", plan.Converted[i].ID, err)
			}
		}
		// Keep ids from ever being reused: AUTOINCREMENT continues after
		// the file's next_id.
		if _, err := tx.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name = 'tasks'`); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq) VALUES ('tasks', ?)`, plan.NextID-1); err != nil {
			return false, err
		}
		if plan.LastSync != "" {
			if err := setTaskMeta(ctx, tx, "last_sync", plan.LastSync); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return plan, err
	}
	return plan, d.ensureProjects(ctx, plan.Create...)
}

// TaskList is the list for display: bucketed, ordered, with the rev.
func (d *DB) TaskList(ctx context.Context, includeArchived bool) (model.TaskList, error) {
	ts, err := d.ListTasks(ctx, includeArchived)
	if err != nil {
		return model.TaskList{}, err
	}
	rev, err := d.TasksRev(ctx)
	if err != nil {
		return model.TaskList{}, err
	}
	return tasks.List(ts, rev, time.Now()), nil
}

// TaskContext is what an automated run starts from (tasks.BuildContext).
func (d *DB) TaskContext(ctx context.Context) (tasks.Context, error) {
	ts, err := d.ListTasks(ctx, false)
	if err != nil {
		return tasks.Context{}, err
	}
	last, err := d.LastTaskSync(ctx)
	if err != nil {
		return tasks.Context{}, err
	}
	return tasks.BuildContext(ts, last, time.Now()), nil
}
