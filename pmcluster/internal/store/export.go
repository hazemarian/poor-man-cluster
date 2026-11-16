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
// left to run) and every row of exactly the named tables, copied verbatim in
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
	plans := make([]CopyPlan, 0, len(tables))
	for _, t := range tables {
		plans = append(plans, CopyPlan{Table: t, Selector: CopyAll})
	}
	return Export(srcPath, dstPath, plans)
}

// Selector chooses how many rows of a table an export carries.
type Selector int

const (
	// CopyAll copies every row of the table.
	CopyAll Selector = iota
	// CopyNewestStackRevisions copies the newest Keep revisions of every
	// stack AND each stack's current_revision row. The current row is not
	// always among the newest ones — a rollback re-points current_revision at
	// an older revision — and the rendered manifest behind that pointer is
	// what a promoted leader needs in order to reconcile or roll back.
	CopyNewestStackRevisions
	// CopyNewestConfigVersions copies the newest Keep versions of every
	// config (highest version-row id first), so config rollback keeps a short
	// window on a promoted leader instead of no history at all.
	CopyNewestConfigVersions
	// CopyReplace copies every row with INSERT OR REPLACE, for a table whose
	// DESTINATION is already seeded by the migrations: the source must win on a
	// primary-key clash instead of failing the whole export.
	//
	// It is how cluster_version crosses — the migration seeds one row (id = 1), so a
	// plain CopyAll would abort with a UNIQUE violation and the kit would stop
	// publishing entirely. Carrying the counter is what keeps a promoted leader's
	// version describing the data it actually holds, instead of restarting at 1 and
	// making every follower re-fetch content it already has.
	CopyReplace
)

// CopyPlan names one table to carry into an export, and how much of it.
//
// The two history selectors exist because those tables grow without bound on a
// live cluster (one row per config edit, one per deploy) while a promoted
// leader needs only a rollback window, not the archive. Keep is a per-group
// cap, never a global row count: "the newest ten configs" would be one config's
// history and nothing else.
type CopyPlan struct {
	Table    string
	Selector Selector
	// Keep is the per-group row cap for the history selectors. It is ignored
	// by CopyAll and must be positive otherwise — a zero that quietly meant
	// "all rows" is the failure these plans exist to prevent.
	Keep int
}

// Export writes a NEW SQLite database at dstPath with srcPath's schema and the
// rows selected by plans, in plan order (parents before children).
//
// Every statement is composed here from the closed Selector set and an
// identifier-checked table name, so a plan chooses a policy and cannot inject
// SQL.
func Export(srcPath, dstPath string, plans []CopyPlan) (map[string]int64, error) {
	if len(plans) == 0 {
		return nil, errors.New("export DB: no tables named")
	}
	for _, p := range plans {
		if !plainIdent(p.Table) {
			return nil, fmt.Errorf("export DB: unsafe table name %q", p.Table)
		}
		switch p.Selector {
		case CopyAll:
		case CopyNewestStackRevisions:
			if p.Table != "stack_revisions" {
				return nil, fmt.Errorf("export DB: newest-stack-revision cap cannot be applied to table %q", p.Table)
			}
			if p.Keep < 1 {
				return nil, fmt.Errorf("export DB: newest-stack-revision cap must be positive, got %d", p.Keep)
			}
		case CopyNewestConfigVersions:
			if p.Table != "config_versions" {
				return nil, fmt.Errorf("export DB: newest-config-version cap cannot be applied to table %q", p.Table)
			}
			if p.Keep < 1 {
				return nil, fmt.Errorf("export DB: newest-config-version cap must be positive, got %d", p.Keep)
			}
		case CopyReplace:
			// Only for a table the destination migrations pre-seed. Allowing it
			// generally would let an export silently overwrite the destination's own
			// rows for any table someone felt like naming.
			if p.Table != "cluster_version" {
				return nil, fmt.Errorf("export DB: replace-copy is for the pre-seeded cluster_version table, not %q", p.Table)
			}
		default:
			return nil, fmt.Errorf("export DB: unknown selector %d for table %q", p.Selector, p.Table)
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

	counts := make(map[string]int64, len(plans))
	for _, p := range plans {
		n, err := copyTable(tx, p)
		if err != nil {
			return nil, err
		}
		counts[p.Table] = n
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

// copyTable runs one plan inside the export transaction and returns how many
// rows it wrote.
func copyTable(tx *sql.Tx, p CopyPlan) (int64, error) {
	switch p.Selector {
	case CopyAll:
		// SELECT * on both sides: the destination schema came from the same
		// migrations as the source, so the columns line up by position. A
		// schema mismatch fails loudly here (column count) instead of
		// silently writing defaults.
		return execCount(tx, p.Table,
			`INSERT INTO main.`+p.Table+` SELECT * FROM src.`+p.Table)

	case CopyNewestStackRevisions:
		// The newest Keep revisions of each stack. ROW_NUMBER is partitioned
		// by stack so the cap is per stack, not per table.
		newest, err := execCount(tx, p.Table, `
			INSERT INTO main.stack_revisions
			       (stack_name, revision, source_yaml, rendered_yaml, rendered_hash,
			        source_file, payload_json, created_at)
			SELECT stack_name, revision, source_yaml, rendered_yaml, rendered_hash,
			       source_file, payload_json, created_at
			  FROM (SELECT r.*,
			               ROW_NUMBER() OVER (PARTITION BY r.stack_name
			                                  ORDER BY r.revision DESC) AS rn
			          FROM src.stack_revisions r)
			 WHERE rn <= ?`, p.Keep)
		if err != nil {
			return 0, err
		}
		// ...plus the row current_revision points at, which is NOT always one
		// of them: a rollback re-points the stack at an older revision while
		// the newer rows stay in history. INSERT OR IGNORE because it usually
		// IS one of them. A stack whose current row is missing from source
		// (it cannot be, given the FK, but a hand-edited file could) simply
		// contributes nothing here and the copy still succeeds.
		current, err := execCount(tx, p.Table, `
			INSERT OR IGNORE INTO main.stack_revisions
			       (stack_name, revision, source_yaml, rendered_yaml, rendered_hash,
			        source_file, payload_json, created_at)
			SELECT r.stack_name, r.revision, r.source_yaml, r.rendered_yaml,
			       r.rendered_hash, r.source_file, r.payload_json, r.created_at
			  FROM src.stack_revisions r
			  JOIN src.stacks s ON s.name = r.stack_name
			 WHERE s.current_revision = r.revision`)
		if err != nil {
			return 0, err
		}
		return newest + current, nil

	case CopyNewestConfigVersions:
		// The newest Keep version rows of each config. Row ids are copied as
		// they are: config rollback names a version row by id, so the ids a
		// promoted leader offers in the console must be the leader's own.
		return execCount(tx, p.Table, `
			INSERT INTO main.config_versions
			SELECT id, config_id, content, hash, created_at
			  FROM (SELECT c.*,
			               ROW_NUMBER() OVER (PARTITION BY c.config_id
			                                  ORDER BY c.id DESC) AS rn
			          FROM src.config_versions c)
			 WHERE rn <= ?`, p.Keep)

	case CopyReplace:
		// SELECT * on both sides for the same reason as CopyAll: the destination
		// schema came from the same migrations, so the columns line up by position and
		// a mismatch fails loudly. OR REPLACE is the whole point: the destination row
		// is the migration's seed, and the source's counter is the truth.
		return execCount(tx, p.Table,
			`INSERT OR REPLACE INTO main.`+p.Table+` SELECT * FROM src.`+p.Table)
	}
	return 0, fmt.Errorf("export DB: unknown selector %d for table %q", p.Selector, p.Table)
}

// execCount runs one copy statement and returns the rows it affected, naming
// the table in any error.
func execCount(tx *sql.Tx, table, query string, args ...any) (int64, error) {
	res, err := tx.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("export DB: copy table %s: %w", table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("export DB: count rows of %s: %w", table, err)
	}
	return n, nil
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
