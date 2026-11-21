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
	"sync"

	"github.com/rs/zerolog"

	_ "modernc.org/sqlite"
)

// ErrNotFound is the generic not-found sentinel for rows that do not have a
// domain-specific error (e.g. the control loop's status snapshots).
var ErrNotFound = errors.New("not found")

// ErrClosed is returned by Swap when the store has no open handle to begin
// with (constructed but never opened, or already closed).
var ErrClosed = errors.New("store is closed")

// Store wraps *sql.DB and runs migrations on Open. Per-resource methods
// live in sibling files.
//
// The handle is swap-able: a manager that gains Swarm leadership replaces its
// control-plane database (data.db + .encryption_key) with the cluster's newest
// snapshot, and the swap must land under every consumer that already holds this
// *Store (the daemon passes one pointer into the HTTP server, the reconciler
// and the CLI services). Swap therefore keeps the *Store identity stable and
// swaps the underlying *sql.DB — see Swap.
// Path returns the store's database file. It exists for the per-installation side files that must be
// found by every process on this host without another setting to keep in sync — today the per-stack
// deploy locks.
func (s *Store) Path() string { return s.path }

type Store struct {
	// mu guards db. Every query takes it for the read side just long enough to
	// read the current handle; Swap takes the write side for the whole
	// close/replace/reopen sequence.
	mu   sync.RWMutex
	db   *sql.DB
	path string
	// closed marks a handle that has been closed. The *sql.DB is deliberately
	// kept (not nilled) so a query on a closed store returns sql.ErrConnDone —
	// the pre-swap behaviour consumers already handle — instead of dereferencing
	// nil.
	closed bool

	// Log is the sink for best-effort diagnostics from store methods that
	// swallow a non-"not found" error (SettingDefault). Defaults to
	// zerolog.Nop(); callers that already hold a logger may wire it.
	Log zerolog.Logger
}

// OpenExisting opens the SQLite DB at dbPath and REFUSES when the file is not
// there, where Open would create it.
//
// The distinction matters on a node that was never initialised: an empty database
// makes every "run pmcluster init first" guard pass, so `doctor` reports a healthy
// store and a manager with no stacks reads as a manager with no stacks. Every
// caller that only wants to READ a setting should use this one; Open is for init
// and join, which are the two places a database is meant to come into being.
func OpenExisting(dbPath string) (*Store, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("no control-plane database at %s yet: %w", dbPath, err)
	}
	return Open(dbPath)
}

// Open opens (or creates) the SQLite DB, applies migrations, and returns
// a ready *Store. Parent directories get mode 0700.
func Open(dbPath string) (*Store, error) {
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, path: dbPath, Log: zerolog.Nop()}, nil
}

// openDB opens the connection, pins it to one connection and applies
// migrations. Shared by Open and Swap so a reopened handle is identical.
func openDB(dbPath string) (*sql.DB, error) {
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
	return db, nil
}

// conn returns the current database handle. Callers hold it only for the
// duration of one statement: a Swap that lands in between closes it, and the
// statement then fails with sql.ErrConnDone / "database is closed" rather than
// reading a file that changed underneath it — the failure mode is an error, not
// a torn read.
func (s *Store) conn() *sql.DB {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db
}

// Close is safe to call multiple times.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil || s.closed {
		return nil
	}
	err := s.db.Close()
	s.closed = true
	return err
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
func (s *Store) DB() *sql.DB { return s.conn() }
