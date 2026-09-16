package store

import (
	"path/filepath"
	"testing"
)

// TestOpen_AppliesMigrations is a smoke test verifying Open creates the DB
// file, runs the embedded migrations, and the bootstrap tables exist.
// Sub-agent (Phase 1.7) should expand this with: idempotent reopen,
// migration ordering, partial-failure rollback, etc.
func TestOpen_AppliesMigrations(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&count); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if count == 0 {
		t.Fatal("schema_version is empty — no migrations were applied")
	}

	var users int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("query users: %v", err)
	}
	if users != 0 {
		t.Fatalf("expected empty users table, got %d rows", users)
	}
}

// TestOpen_Idempotent verifies reopening an existing DB does not re-run
// already-applied migrations and does not error.
func TestOpen_Idempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	var first int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&first); err != nil {
		t.Fatalf("query: %v", err)
	}
	_ = s.Close()

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	var second int
	if err := s2.DB().QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&second); err != nil {
		t.Fatalf("query: %v", err)
	}
	if first != second {
		t.Fatalf("schema_version row count changed across reopens: %d → %d", first, second)
	}
}
