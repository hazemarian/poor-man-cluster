package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// openRawDB opens a plain SQLite DB (without running migrations) for migration
// unit tests that need to inspect schema_version state at a fine-grained level.
func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	dsn := "file:" + dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestRunMigrations_Idempotent verifies that calling runMigrations multiple
// times on the same DB does not re-apply already-applied migrations.
func TestRunMigrations_Idempotent(t *testing.T) {
	db := openRawDB(t)

	for i := 0; i < 3; i++ {
		if err := runMigrations(db); err != nil {
			t.Fatalf("runMigrations (pass %d): %v", i+1, err)
		}
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&count); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}

	if count == 0 {
		t.Fatal("schema_version is empty after runMigrations")
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations (4th pass): %v", err)
	}
	var count2 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&count2); err != nil {
		t.Fatalf("query schema_version (2nd): %v", err)
	}
	if count != count2 {
		t.Errorf("schema_version count grew from %d to %d across idempotent reruns", count, count2)
	}
}

// TestRunMigrations_LexOrder verifies that applied migrations are recorded
// with version strings matching the embedded file names (minus the .sql
// suffix) and that when there is more than one migration they appear in
// lexicographic order. With the current single-file corpus this ensures the
// naming convention is respected; future migrations added without a numeric
// prefix will be caught.
func TestRunMigrations_LexOrder(t *testing.T) {
	db := openRawDB(t)

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	rows, err := db.Query(`SELECT version FROM schema_version ORDER BY rowid`)
	if err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	defer rows.Close()

	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(versions) == 0 {
		t.Fatal("no versions recorded in schema_version")
	}

	for i := 1; i < len(versions); i++ {
		if versions[i] < versions[i-1] {
			t.Errorf("schema_version order violated: %q comes after %q",
				versions[i], versions[i-1])
		}
	}

	if versions[0] != "0001_init" {
		t.Errorf("first schema_version version = %q, want %q", versions[0], "0001_init")
	}
}

// TestRunMigrations_SchemaVersion_MatchesFiles verifies that the version name
// recorded in schema_version equals the file name without the .sql suffix,
// which is the convention used by runMigrations and loadAppliedVersions.
func TestRunMigrations_SchemaVersion_MatchesFiles(t *testing.T) {
	db := openRawDB(t)

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	files, err := listMigrations()
	if err != nil {
		t.Fatalf("listMigrations: %v", err)
	}

	var recordedVersions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&recordedVersions); err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if recordedVersions != len(files) {
		t.Errorf("schema_version has %d rows, want %d (one per *.sql file)",
			recordedVersions, len(files))
	}

	for _, f := range files {
		version := f[:len(f)-len(".sql")]
		var applied int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM schema_version WHERE version = ?`, version,
		).Scan(&applied); err != nil {
			t.Fatalf("query for version %q: %v", version, err)
		}
		if applied != 1 {
			t.Errorf("version %q: got %d rows in schema_version, want 1", version, applied)
		}
	}
}

// TestApplyMigration_MalformedBodyRolledBack verifies that a migration with
// invalid SQL is NOT recorded in schema_version. The caller can retry once the
// body is fixed; a failed migration leaves the DB at its previous state.
func TestApplyMigration_MalformedBodyRolledBack(t *testing.T) {
	db := openRawDB(t)

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL) STRICT`); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}

	badSQL := []byte(`THIS IS NOT VALID SQL !!!`)
	err := applyMigration(db, "test_bad", badSQL)
	if err == nil {
		t.Fatal("expected error from applyMigration with malformed SQL, got nil")
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version = 'test_bad'`).Scan(&count); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if count != 0 {
		t.Errorf("malformed migration was recorded in schema_version (count=%d), want 0", count)
	}
}

// TestApplyMigration_HappyPath verifies that a well-formed migration body is
// executed and recorded atomically.
func TestApplyMigration_HappyPath(t *testing.T) {
	db := openRawDB(t)

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL) STRICT`); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}

	goodSQL := []byte(`CREATE TABLE test_table (id INTEGER PRIMARY KEY) STRICT`)
	if err := applyMigration(db, "test_good", goodSQL); err != nil {
		t.Fatalf("applyMigration: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version = 'test_good'`).Scan(&count); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 schema_version row for test_good, got %d", count)
	}

	if _, err := db.Exec(`INSERT INTO test_table (id) VALUES (1)`); err != nil {
		t.Errorf("table test_table was not created: %v", err)
	}
}
