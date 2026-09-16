// Package store persists the UI's own data (users + settings) in SQLite.
//
// This is deliberately separate from the pmcluster daemon's store: the UI can
// live anywhere (in or out of the swarm) and only ever talks to the cluster
// through the pmcluster API. Here we keep only what the UI itself needs — who
// may log in, and the operator's preferred API URL/token overrides.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a user or setting doesn't exist.
var ErrNotFound = errors.New("not found")

// Settings keys for the console's pmcluster API override (the defaults come
// from env / mounted Swarm secrets; these let the operator override in the UI,
// or are seeded once on first boot).
const (
	KeyAPIURL = "pmcluster_api_url"
	KeyToken  = "pmcluster_api_token"
)

// Store wraps the SQLite connection. All access is serialized by SQLite's
// default; a single admin UI client makes contention negligible.
type Store struct {
	db *sql.DB
}

// User is a UI login. PasswordHash is a bcrypt hash; PasswordSet is true once a
// password has been chosen (either from env bootstrap or the first-load setup).
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	PasswordSet  bool
	CreatedAt    int64
}

// Open creates (if needed) and opens the SQLite DB under dataDir.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir data dir: %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "pmcluster-ui.db"))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		PRAGMA journal_mode=WAL;
		CREATE TABLE IF NOT EXISTS users (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			username      TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL DEFAULT '',
			password_set  INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying connection.
func (s *Store) Close() error { return s.db.Close() }

// GetByUsername returns a user or ErrNotFound.
func (s *Store) GetByUsername(ctx context.Context, username string) (*User, error) {
	u := &User{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, password_set, created_at
		   FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PasswordSet, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// FirstUser returns the lowest-ID user (the installer/admin), or ErrNotFound.
func (s *Store) FirstUser(ctx context.Context) (*User, error) {
	u := &User{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, password_set, created_at
		   FROM users ORDER BY id LIMIT 1`).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PasswordSet, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// CountUsers returns the number of stored users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts a user and returns the stored row.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, passwordSet bool) (*User, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, password_set, created_at)
		 VALUES (?, ?, ?, ?)`, username, passwordHash, boolInt(passwordSet), time.Now().Unix()); err != nil {
		return nil, err
	}
	return s.GetByUsername(ctx, username)
}

// SetPassword sets (or replaces) a password hash and marks it set.
func (s *Store) SetPassword(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, password_set = 1 WHERE username = ?`,
		passwordHash, username)
	return err
}

// CreateEnvUser seeds the env-configured user if it doesn't already exist.
// Returns true if a new user was created. PasswordSetup is always true here.
func (s *Store) CreateEnvUser(ctx context.Context, username, passwordHash string) (bool, error) {
	if _, err := s.GetByUsername(ctx, username); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if _, err := s.CreateUser(ctx, username, passwordHash, true); err != nil {
		return false, err
	}
	return true, nil
}

// GetSetting returns a setting value or ErrNotFound.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// GetSettings returns all settings as a map.
func (s *Store) GetSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
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

// SetSetting upserts a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SeedSettingOnce writes value under key only if the key has never been set.
// This is how the API token provisioned by pmcluster is "stored once": it lands
// in the console DB on first boot and is never re-seeded, so an operator's
// explicit clear from Settings (SetSetting(key, "")) or subsequent edit is
// respected and not overwritten on the next start.
func (s *Store) SeedSettingOnce(ctx context.Context, key, value string) error {
	if _, err := s.GetSetting(ctx, key); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	return s.SetSetting(ctx, key, value)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
