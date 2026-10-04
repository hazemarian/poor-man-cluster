// Package store wraps a pure-Go SQLite database for pmcluster state:
// users, managed credentials, stack revisions, registry credentials,
// webhook secrets, backup metadata.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// ErrNotFound is the generic not-found sentinel for rows that do not have a
// domain-specific error (e.g. the control loop's status snapshots).
var ErrNotFound = errors.New("not found")

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
	// journal_mode=DELETE (the rollback journal) is deliberate: WAL mode
	// corrupts the control-plane DB under multi-process access. When a
	// second process (the CLI, python3, ...) opens the same database and
	// closes its connection, SQLite deletes the -wal/-shm sidecars even
	// though the long-running daemon still holds them open. The daemon then
	// keeps writing to the orphaned inode, producing a split-brain database
	// (stale reads, torn indexes, invisible writes). The rollback journal
	// commits directly to the main file and has no long-lived sidecars, so
	// concurrent daemon+CLI access stays consistent.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(DELETE)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", dbPath)
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

// WALCheckpoint keeps the BackupTrigger contract but is a no-op: the store
// uses the rollback journal (journal_mode=DELETE), so there is no WAL to
// flush — transactions commit directly to the main database file, which is
// exactly the consistent state volume-level snapshots need.
func (s *Store) WALCheckpoint(ctx context.Context) error {
	// journal_mode=DELETE: nothing to checkpoint. Executing
	// "PRAGMA wal_checkpoint(TRUNCATE)" in DELETE mode is a harmless no-op
	// returning (0,0,0), but skipping it avoids confusion.
	return nil
}

// DB is for tests and migration inspection — production code uses the
// typed methods on Store.
func (s *Store) DB() *sql.DB { return s.db }
