package db

import (
	"context"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// Messages between sessions (docs/decisions/0009-orchestrator.md §13): the
// receiving collector's inbox. A row is written when the message arrives
// and stamped when it has been handed to the session's claude; nothing
// here deletes one, so a restart loses none and the record stays.

// InsertMessage queues a message for m.SessionID. The caller redacts
// m.Text.
func (d *DB) InsertMessage(ctx context.Context, m model.Message) (int64, error) {
	at := m.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	res, err := d.sql.ExecContext(ctx, `INSERT INTO messages (session_id, from_session, from_ref, from_host, text, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, m.SessionID, m.FromSession, m.FromRef, m.FromHost, m.Text, at.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const messageCols = `id, session_id, COALESCE(from_session,''), COALESCE(from_ref,''), COALESCE(from_host,''), text,
	created_at, COALESCE(delivered_at,0), COALESCE(how,'')`

func (d *DB) queryMessages(ctx context.Context, q string, args ...any) ([]model.Message, error) {
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Message{}
	for rows.Next() {
		var m model.Message
		var at, done int64
		if err := rows.Scan(&m.ID, &m.SessionID, &m.FromSession, &m.FromRef, &m.FromHost, &m.Text, &at, &done, &m.How); err != nil {
			return nil, err
		}
		m.CreatedAt = time.UnixMilli(at).UTC()
		if done > 0 {
			m.DeliveredAt = time.UnixMilli(done).UTC()
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PendingMessages returns every message not delivered yet, oldest first.
func (d *DB) PendingMessages(ctx context.Context) ([]model.Message, error) {
	return d.queryMessages(ctx, `SELECT `+messageCols+` FROM messages WHERE delivered_at IS NULL ORDER BY id`)
}

// SessionMessages returns the messages sent to one session, oldest first.
func (d *DB) SessionMessages(ctx context.Context, sessionID string) ([]model.Message, error) {
	return d.queryMessages(ctx, `SELECT `+messageCols+` FROM messages WHERE session_id = ? ORDER BY id`, sessionID)
}

// Message returns one message by id.
func (d *DB) Message(ctx context.Context, id int64) (model.Message, bool, error) {
	ms, err := d.queryMessages(ctx, `SELECT `+messageCols+` FROM messages WHERE id = ?`, id)
	if err != nil || len(ms) == 0 {
		return model.Message{}, false, err
	}
	return ms[0], true, nil
}

// MarkMessageDelivered stamps a message as handed to claude (how:
// model.MessageTyped / MessageResumed). A message already stamped keeps
// its first stamp.
func (d *DB) MarkMessageDelivered(ctx context.Context, id int64, how string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE messages SET delivered_at = ?, how = ? WHERE id = ? AND delivered_at IS NULL`,
		time.Now().UnixMilli(), how, id)
	return err
}
