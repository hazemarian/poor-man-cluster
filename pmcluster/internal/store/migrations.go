package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/migrations"
)

// runMigrations applies every embedded *.sql file under pmcluster/migrations
// in lexicographic order, recording each applied version in schema_version.
// Already-applied versions are skipped. Each file runs in its own
// transaction so a failure mid-file leaves the DB at the previous version.
func runMigrations(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("ensure schema_version table: %w", err)
	}

	applied, err := loadAppliedVersions(db)
	if err != nil {
		return err
	}

	files, err := listMigrations()
	if err != nil {
		return err
	}

	for _, name := range files {
		version := strings.TrimSuffix(name, ".sql")
		if applied[version] {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if err := applyMigration(db, version, body); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

func loadAppliedVersions(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`SELECT version FROM schema_version`)
	if err != nil {
		return nil, fmt.Errorf("query schema_version: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_version: %w", err)
		}
		out[v] = true
	}
	return out, rows.Err()
}

func listMigrations() ([]string, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

func applyMigration(db *sql.DB, version string, body []byte) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(string(body)); err != nil {
		return fmt.Errorf("exec migration body: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`, version, time.Now().Unix()); err != nil {
		return fmt.Errorf("record schema_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
