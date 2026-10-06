package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Device is one registered machine. TokenHash is the sha256 of the device
// token; the token itself is shown once at creation and never stored.
type Device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen,omitzero"`
	Hostname  string    `json:"hostname,omitempty"`
	Version   string    `json:"version,omitempty"`
	tokenHash string
}

// store is the hub's own SQLite file — separate from any collector DB. It
// holds the device registry and a small kv table (the cookie signing key).
type store struct {
	db *sql.DB
}

func openStore(dir string) (*store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "hub.sqlite")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS devices (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			token_hash TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL,
			last_seen TEXT NOT NULL DEFAULT '',
			hostname TEXT NOT NULL DEFAULT '',
			version TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS push_subs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT NOT NULL,
			endpoint TEXT NOT NULL UNIQUE,
			p256dh TEXT NOT NULL,
			auth TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("hub migrate: %w", err)
		}
	}
	return &store{db: db}, nil
}

func (s *store) Close() error { return s.db.Close() }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// secret returns the persisted value for k, creating it with gen on first use.
func (s *store) secret(ctx context.Context, k string, gen func() string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = ?`, k).Scan(&v)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	v = gen()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO kv (k, v) VALUES (?, ?)`, k, v); err != nil {
		return "", err
	}
	return v, nil
}

func (s *store) putKV(ctx context.Context, k, v string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// ErrDuplicateName is returned when a device name is already taken.
var ErrDuplicateName = errors.New("a device with that name already exists")

// createDevice registers name and returns the device plus its one-time token.
func (s *store) createDevice(ctx context.Context, name string) (Device, string, error) {
	d := Device{ID: randHex(8), Name: name, CreatedAt: time.Now().UTC()}
	tok := "ccdh_" + randHex(32)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (id, name, token_hash, created_at) VALUES (?, ?, ?, ?)`,
		d.ID, d.Name, hashToken(tok), d.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") && strings.Contains(err.Error(), "name") {
			return Device{}, "", ErrDuplicateName
		}
		return Device{}, "", err
	}
	return d, tok, nil
}

// rotateToken replaces a device's token, cutting off the old one.
func (s *store) rotateToken(ctx context.Context, id string) (string, error) {
	tok := "ccdh_" + randHex(32)
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET token_hash = ? WHERE id = ?`, hashToken(tok), id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", sql.ErrNoRows
	}
	return tok, nil
}

func (s *store) deleteDevice(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *store) renameDevice(ctx context.Context, id, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrDuplicateName
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

const deviceCols = `id, name, token_hash, created_at, last_seen, hostname, version`

func scanDevice(sc interface{ Scan(...any) error }) (Device, error) {
	var d Device
	var created, seen string
	if err := sc.Scan(&d.ID, &d.Name, &d.tokenHash, &created, &seen, &d.Hostname, &d.Version); err != nil {
		return Device{}, err
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	d.LastSeen, _ = time.Parse(time.RFC3339Nano, seen)
	return d, nil
}

func (s *store) listDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// deviceByToken authenticates a tunnel dial.
func (s *store) deviceByToken(ctx context.Context, tok string) (Device, error) {
	return scanDevice(s.db.QueryRowContext(ctx,
		`SELECT `+deviceCols+` FROM devices WHERE token_hash = ?`, hashToken(tok)))
}

func (s *store) touchDevice(ctx context.Context, id, hostname, version string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE devices SET last_seen = ?, hostname = ?, version = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), hostname, version, id)
	return err
}
