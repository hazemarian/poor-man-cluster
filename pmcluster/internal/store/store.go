// Package store wraps a pure-Go SQLite database for pmcluster state:
// users, managed credentials, stack revisions, registry credentials,
// webhook secrets, backup metadata.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store wraps *sql.DB and runs migrations on Open. Per-resource methods
// live in sibling files.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite DB, applies migrations, and returns
// a ready *Store. Parent directories get mode 0700.
func Open(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("ensure data dir: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(1)

	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	if err := runMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return &Store{db: db}, nil
}

// Close is safe to call multiple times.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// WALCheckpoint flushes all pending WAL transactions to the main
// database file so volume-level snapshots capture a consistent state.
// Uses TRUNCATE to also reset the WAL file size (ideal before backups).
func (s *Store) WALCheckpoint(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// DB is for tests and migration inspection — production code uses the
// typed methods on Store.
func (s *Store) DB() *sql.DB { return s.db }
