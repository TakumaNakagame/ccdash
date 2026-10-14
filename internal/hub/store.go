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
	// PeerAccess lets this device reach the others with its token (peer.go):
	// "" off, "read", "control".
	PeerAccess string `json:"peer_access,omitempty"`
	// Letter prefixes the device's session numbers ("A42") so a session
	// handle is unique across machines. Assigned in registration order,
	// the operator may change it.
	Letter    string `json:"letter,omitempty"`
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
	return openStoreFile(filepath.Join(dir, "hub.sqlite"))
}

func openStoreFile(path string) (*store, error) {
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
	for _, q := range []string{
		`ALTER TABLE devices ADD COLUMN peer_access TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE devices ADD COLUMN letter TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = db.Close()
			return nil, fmt.Errorf("hub migrate: %w", err)
		}
	}
	st := &store{db: db}
	if err := st.backfillLetters(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("hub migrate: letters: %w", err)
	}
	return st, nil
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
	used, err := s.usedLetters(ctx)
	if err != nil {
		return Device{}, "", err
	}
	d.Letter = nextLetter(used)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO devices (id, name, token_hash, created_at, letter) VALUES (?, ?, ?, ?, ?)`,
		d.ID, d.Name, hashToken(tok), d.CreatedAt.Format(time.RFC3339Nano), d.Letter)
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

const deviceCols = `id, name, token_hash, created_at, last_seen, hostname, version, peer_access, letter`

func scanDevice(sc interface{ Scan(...any) error }) (Device, error) {
	var d Device
	var created, seen string
	if err := sc.Scan(&d.ID, &d.Name, &d.tokenHash, &created, &seen, &d.Hostname, &d.Version, &d.PeerAccess, &d.Letter); err != nil {
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

func (s *store) setPeerAccess(ctx context.Context, id, access string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET peer_access = ? WHERE id = ?`, access, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ErrDuplicateLetter is returned when another device has the letter.
var ErrDuplicateLetter = errors.New("another device already has that letter")

// validLetter: one or two capital letters (A–Z, then AA… past 26 devices).
func validLetter(l string) bool {
	if len(l) < 1 || len(l) > 2 {
		return false
	}
	for _, c := range l {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// nextLetter is the first unused of A…Z, AA…ZZ.
func nextLetter(used map[string]bool) string {
	for a := 'A'; a <= 'Z'; a++ {
		if l := string(a); !used[l] {
			return l
		}
	}
	for a := 'A'; a <= 'Z'; a++ {
		for b := 'A'; b <= 'Z'; b++ {
			if l := string([]rune{a, b}); !used[l] {
				return l
			}
		}
	}
	return ""
}

func (s *store) usedLetters(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT letter FROM devices WHERE letter != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[string]bool{}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		used[l] = true
	}
	return used, rows.Err()
}

// backfillLetters gives devices registered before letters existed one each,
// in registration order (the first machine is A).
func (s *store) backfillLetters(ctx context.Context) error {
	used, err := s.usedLetters(ctx)
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM devices WHERE letter = '' AND id != ? ORDER BY created_at, id`, LocalDeviceID)
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
	for _, id := range ids {
		l := nextLetter(used)
		if _, err := s.db.ExecContext(ctx, `UPDATE devices SET letter = ? WHERE id = ?`, l, id); err != nil {
			return err
		}
		used[l] = true
	}
	return nil
}

func (s *store) setLetter(ctx context.Context, id, letter string) error {
	var other string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM devices WHERE letter = ? AND id != ?`, letter, id).Scan(&other)
	if err == nil {
		return ErrDuplicateLetter
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET letter = ? WHERE id = ?`, letter, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
