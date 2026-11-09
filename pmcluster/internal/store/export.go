package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// ExportDB writes a NEW SQLite database at dstPath holding the same schema as
// srcPath (all migrations applied, so the file opens through Open with nothing
// left to run) and the rows of exactly the named tables, copied verbatim in
// the order given.
//
// It exists for the control-plane survivor kit: the kit is Raft-replicated to
// every manager as an immutable Docker config, capped at 500 KiB, and the
// leader's data.db had grown past that cap purely in stack_revisions (5.2 MB of
// file, 538 KB gzipped — every publish refused for days, so keys and settings
// created since the last good snapshot existed only on one disk). Packing an
// export of the AUTHORITATIVE tables instead keeps the kit small while still
// carrying auth, settings, credential ciphertexts and the stack list.
//
// This is deliberately not a backup tool: it drops tables the caller does not
// name, and the full data.db still rides the tarball/offsite path. Parents must
// precede children in tables, because foreign keys are enforced during the copy.
//
// The copy runs in one transaction, so the rows come from one read snapshot of
// src rather than from whatever state each table happened to be in; a daemon
// writing during the export cannot tear the UUID/credential relationships
// across tables.
func ExportDB(srcPath, dstPath string, tables []string) (map[string]int64, error) {
	if len(tables) == 0 {
		return nil, errors.New("export DB: no tables named")
	}
	for _, t := range tables {
		if !plainIdent(t) {
			return nil, fmt.Errorf("export DB: unsafe table name %q", t)
		}
	}

	// A source that is not a database we can read is reported here, before a
	// destination file exists — the caller can then decide to fall back.
	if _, err := os.Stat(srcPath); err != nil {
		return nil, fmt.Errorf("export DB: source: %w", err)
	}

	// Schema first, through the same path the daemon opens a database with, so
	// the exported file is one this binary migrates to a no-op.
	st, err := Open(dstPath)
	if err != nil {
		return nil, fmt.Errorf("export DB: create destination schema: %w", err)
	}
	if err := st.Close(); err != nil {
		return nil, fmt.Errorf("export DB: close destination: %w", err)
	}

	// A half-written export is worse than none: the caller's fallback decides
	// what to do only when it gets an error, and a 3-page file named data.db
	// looks like a database. Registered before the handle's Close so the handle
	// is closed first on the way out.
	complete := false
	defer func() {
		if complete {
			return
		}
		_ = os.Remove(dstPath)
		_ = os.Remove(dstPath + "-journal")
	}()

	db, err := sql.Open("sqlite", sqliteURI(dstPath, "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"))
	if err != nil {
		return nil, fmt.Errorf("export DB: open destination: %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	// mode=ro: the leader's database is open for writing in the daemon, and an
	// export must never be able to roll a journal into it.
	if _, err := db.Exec(`ATTACH DATABASE ? AS src`, sqliteURI(srcPath, "mode=ro")); err != nil {
		return nil, fmt.Errorf("export DB: attach source: %w", err)
	}
	attached := true
	defer func() {
		if attached {
			_, _ = db.Exec(`DETACH DATABASE src`)
		}
	}()

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("export DB: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	counts := make(map[string]int64, len(tables))
	for _, t := range tables {
		// SELECT * on both sides: the destination schema came from the same
		// migrations as the source, so the columns line up by position. A
		// schema mismatch fails loudly here (column count) instead of
		// silently writing defaults.
		res, err := tx.Exec(`INSERT INTO main.` + t + ` SELECT * FROM src.` + t)
		if err != nil {
			return nil, fmt.Errorf("export DB: copy table %s: %w", t, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("export DB: count rows of %s: %w", t, err)
		}
		counts[t] = n
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("export DB: commit: %w", err)
	}
	if _, err := db.Exec(`DETACH DATABASE src`); err != nil {
		return nil, fmt.Errorf("export DB: detach source: %w", err)
	}
	attached = false

	// Rewrite the file so it carries no free pages left by the insert order —
	// the payload is compressed and capped, so a page of slack is worth
	// reclaiming.
	if _, err := db.Exec(`VACUUM`); err != nil {
		return nil, fmt.Errorf("export DB: compact destination: %w", err)
	}
	complete = true
	return counts, nil
}

// sqliteURI builds a modernc.org/sqlite DSN for a file path, escaping it so a
// path with spaces or a '?' stays a path. params is the raw query string
// (empty for none).
func sqliteURI(path, params string) string {
	u := url.URL{Scheme: "file", Path: path}
	u.RawQuery = params
	return u.String()
}

// plainIdent reports whether name is safe to interpolate as a table identifier.
// These names are constants in this repository, but a copy statement built by
// concatenation should not depend on that remaining true.
func plainIdent(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return !strings.HasPrefix(name, "sqlite_")
}
