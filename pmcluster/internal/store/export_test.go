package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestExportDB_CopiesOnlyNamedTables is the primitive the control-plane kit is
// built on: a new database with the full schema and ONLY the named tables'
// rows, openable by the daemon's own store without running a migration.
func TestExportDB_CopiesOnlyNamedTables(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := Open(srcPath)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer func() { _ = src.Close() }()

	if _, err := src.CreateUser(ctx, "admin", "tokid", "hash"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := src.SetSetting(ctx, "domain", "example.com"); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	if _, err := src.CreateBackup(ctx, "", 0); err != nil {
		t.Fatalf("create backup: %v", err)
	}
	if err := src.RecordDeploy(ctx, &StackRevision{
		StackName: "demo", Revision: 10, SourceYAML: "app: demo", RenderedYAML: "services: {}",
	}, ""); err != nil {
		t.Fatalf("record deploy: %v", err)
	}

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	counts, err := ExportDB(srcPath, dstPath, []string{"users", "cluster_settings"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if counts["users"] != 1 || counts["cluster_settings"] != 1 {
		t.Fatalf("export counts = %v, want users=1 cluster_settings=1", counts)
	}

	// Opening through the daemon's store must be a no-op migration: the
	// exported file carries the complete schema and its schema_version rows.
	exported, err := Open(dstPath)
	if err != nil {
		t.Fatalf("open exported database: %v", err)
	}
	defer func() { _ = exported.Close() }()

	if n := countIn(t, exported.DB(), "users"); n != 1 {
		t.Errorf("users = %d, want 1", n)
	}
	if n := countIn(t, exported.DB(), "cluster_settings"); n != 1 {
		t.Errorf("cluster_settings = %d, want 1", n)
	}
	if n := countIn(t, exported.DB(), "backups"); n != 0 {
		t.Errorf("backups = %d, want 0 (table not named)", n)
	}
	if n := countIn(t, exported.DB(), "stack_revisions"); n != 0 {
		t.Errorf("stack_revisions = %d, want 0 (table not named)", n)
	}
	// The stack ROW is what the kit keeps when revisions stay behind.
	if n := countIn(t, exported.DB(), "stacks"); n != 0 {
		t.Errorf("stacks = %d, want 0 (table not named in this export)", n)
	}
	var migrations int
	if err := exported.DB().QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&migrations); err != nil {
		t.Fatalf("schema_version unreadable in the export: %v", err)
	}
	if migrations == 0 {
		t.Error("exported database carries no schema_version rows — it would re-run migrations on open")
	}
}

// TestExportDB_NotADatabase documents the error the kit's fallback hangs on: a
// source that is not a SQLite database is refused BEFORE a destination file is
// produced, so a caller can choose to pack the file verbatim instead.
func TestExportDB_NotADatabase(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "data.db")
	if err := os.WriteFile(srcPath, []byte("sqlite-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	dstPath := filepath.Join(dir, "out.db")
	if _, err := ExportDB(srcPath, dstPath, []string{"users"}); err == nil {
		t.Fatal("exporting a non-database must fail")
	}
	if _, err := os.Stat(dstPath); err == nil {
		t.Error("a failed export must not leave a destination database behind")
	}
}

// TestExportDB_RejectsUnsafeNames keeps the concatenated copy statement honest:
// the table names are constants today, and the guard is what makes that a
// property of the code rather than of everyone's future memory.
func TestExportDB_RejectsUnsafeNames(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	st, err := Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	for _, name := range []string{"users; DROP TABLE users", "sqlite_sequence", "", "users WHERE 1=1"} {
		if _, err := ExportDB(srcPath, filepath.Join(dir, "out.db"), []string{name}); err == nil {
			t.Errorf("ExportDB accepted unsafe table name %q", name)
		}
	}
	if _, err := ExportDB(srcPath, filepath.Join(dir, "out.db"), nil); err == nil {
		t.Error("ExportDB accepted an empty table list")
	}
}

func countIn(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
