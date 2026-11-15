package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The mirror carries the WHOLE control-plane database, not a projection of it.
//
// ADR 0003 started with a projection (settings, secrets, users) because the mirror
// only had to answer reads on a follower. ADR 0005 reverses that: adoption no longer
// copies a file, so a manager promoted to leader has nothing but the store it has
// been mirroring — and a store that carried three tables would promote a leader that
// cannot read its stacks, configs, registries or GitHub App. The mirror is therefore
// the document.
//
// Two lists, both DATA rather than prose, both held to the schema by
// TestMirrorTables_EveryTableIsClassified: a migration that adds a table and
// classifies it nowhere fails the build instead of silently vanishing from every
// manager that is not the leader.
//
//   - mirroredTables: copied to every follower, in this order. Parents precede
//     children, because the apply deletes children first and inserts parents first
//     under foreign_keys(1).
//   - excludedTables: stays on the node that wrote it, with its reason. The test is
//     ONE question — does every row describe the machine it lives on? A row that
//     describes the cluster is cluster state and must survive losing the leader.
var mirroredTables = []string{
	// The content identity a follower mirrors by. Mirrored so a promoted leader keeps
	// describing the state it actually holds instead of restarting at 1 and invalidating
	// every follower's ETag.
	"cluster_version",
	"users",
	"managed_credentials",
	"stacks",
	// After stacks: stack_revisions.stack_name is a foreign key into it.
	"stack_revisions",
	"webhook_sources",
	"registries",
	"cluster_settings",
	"configs",
	// After configs: config_versions.config_id is a foreign key into it.
	"config_versions",
	"secrets",
	"site_certs",
	"stack_failover",
	"github_app",
	"github_broker_secrets",
	// The reconcile loop's own output. Mirrored because it is a leader-written cache
	// that a promoted leader serves from the moment it takes over, before its first
	// pass rebuilds it.
	"stack_status",
	// Delivery history. Mirrored for the same reason stack_revisions are: the console
	// answers "what did CI send us" from the store, and a promoted leader with no rows
	// answers "nothing was ever delivered", which is a lie an operator cannot tell from
	// a quiet repository.
	"webhook_deliveries",
}

// excludedTables names every table that stays out of the mirror entirely, and why.
//
// Each of these is here for the same reason: a row describes the NODE it lives on,
// so carrying it to another machine would make that machine claim something it never
// observed. sync_state is the mirror's own bookkeeping and is never even a candidate.
var excludedTables = map[string]string{
	// One row per archive on THIS node's disk. Every storage node's agent discovers its
	// own archives from the filesystem, and a promoted leader that inherited another
	// node's rows would list archives it cannot read or delete.
	"backups": "node-local archive index; each node rediscovers its own files from disk",
	// Per-node resource samples from the collector. Volatile telemetry, repopulated by
	// the node that produces it within one interval.
	"node_stats": "per-node resource samples; volatile telemetry of the node that measured it",
	// The runner panel's current-state rows, written by the node that owns each runner.
	// A promoted leader observes its own node and rebuilds its row on the first pass;
	// another node's row describes a container this node cannot see or remove.
	"github_runners": "per-node runner observations; each node writes its own row",
	// This node's own mirror bookkeeping: which leader's version IT applied, and when.
	// A promoted leader must NOT adopt another node's copy — it is about to become the
	// writer those rows describe, and inheriting a mirror's "current" claim is how a
	// leader starts lying about its own freshness.
	"sync_state": "this node's mirror bookkeeping; a promoted leader is the writer, not a mirror",
	// Structural, not policy: the migration ledger is written by this node's own
	// Open(), and sqlite_sequence is maintained by SQLite for AUTOINCREMENT tables.
	// Copying either one would describe a schema history (or a row counter) that belongs
	// to the database file, not to the cluster.
	"schema_version":  "the migration ledger; written by this node's own Open()",
	"sqlite_sequence": "maintained by SQLite for AUTOINCREMENT tables",
}

// TableData is one mirrored table exactly as it crosses the wire: the column names in
// order, their declared types (so a BLOB survives JSON as bytes rather than becoming a
// base64-shaped string), and the rows positionally.
//
// Positional rows rather than an object per row: stack_revisions carries whole rendered
// manifests, and repeating twenty column names per row would multiply the payload the
// poll has to move.
type TableData struct {
	Columns []string `json:"columns"`
	Types   []string `json:"types"`
	Rows    [][]any  `json:"rows"`
}

// MirroredTables names the tables a follower mirrors, in apply order (parents first).
// Exported for the schema-classification test and for any surface that has to explain
// what a mirror carries.
func MirroredTables() []string {
	out := make([]string, len(mirroredTables))
	copy(out, mirroredTables)
	return out
}

// ExcludedTables maps every table kept out of the mirror to the reason, with a copy so
// a caller cannot mutate the classification.
func ExcludedTables() map[string]string {
	out := make(map[string]string, len(excludedTables))
	for k, v := range excludedTables {
		out[k] = v
	}
	return out
}

// IsMirrored reports whether name is one of the mirrored tables.
func IsMirrored(name string) bool {
	for _, t := range mirroredTables {
		if t == name {
			return true
		}
	}
	return false
}

// readMirrorTables reads every mirrored table inside the snapshot's transaction, so the
// version and the rows describe the same point in time.
func readMirrorTables(ctx context.Context, tx *sql.Tx) (map[string]TableData, error) {
	out := make(map[string]TableData, len(mirroredTables))
	for _, table := range mirroredTables {
		cols, types, err := tableColumns(ctx, tx, table)
		if err != nil {
			return nil, err
		}
		rows, err := tableRows(ctx, tx, table, len(cols))
		if err != nil {
			return nil, err
		}
		out[table] = TableData{Columns: cols, Types: types, Rows: rows}
	}
	return out, nil
}

// tableColumns reads a table's columns and their declared types.
//
// The identifier is checked rather than trusted: these names are constants in this
// package, but a statement composed by concatenation should not depend on that.
func tableColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, []string, error) {
	if !plainIdent(table) {
		return nil, nil, fmt.Errorf("mirror: unsafe table name %q", table)
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, nil, fmt.Errorf("mirror: columns of %s: %w", table, err)
	}
	defer rows.Close()
	var cols, types []string
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notNull   int
			dflt      any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, nil, fmt.Errorf("mirror: scan column of %s: %w", table, err)
		}
		cols = append(cols, name)
		types = append(types, typ)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("mirror: columns of %s: %w", table, err)
	}
	if len(cols) == 0 {
		return nil, nil, fmt.Errorf("mirror: table %s has no columns (does it exist?)", table)
	}
	return cols, types, nil
}

// tableRows reads every row of a table in rowid order, so the same version always
// produces the same bytes.
func tableRows(ctx context.Context, tx *sql.Tx, table string, width int) ([][]any, error) {
	rows, err := tx.QueryContext(ctx, `SELECT * FROM `+table+` ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("mirror: read %s: %w", table, err)
	}
	defer rows.Close()
	out := [][]any{}
	for rows.Next() {
		vals := make([]any, width)
		ptrs := make([]any, width)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("mirror: scan row of %s: %w", table, err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mirror: read %s: %w", table, err)
	}
	return out, nil
}

// applyMirrorTables replaces the mirrored tables with the document's rows, in one
// transaction: delete children first, insert parents first, stamp mirrored_at where the
// table carries it.
//
// A table the document does not name is left ALONE. That is the rolling-upgrade rule: a
// leader on an older binary sends three tables and a follower keeps the rest, instead of
// deleting rows nobody sent.
func applyMirrorTables(ctx context.Context, tx *sql.Tx, tables map[string]TableData, appliedAt int64) error {
	for name := range tables {
		if !IsMirrored(name) {
			return fmt.Errorf("apply snapshot: the leader sent table %q, which this binary does not mirror (version skew: this node's schema is older than the leader's)", name)
		}
	}
	// Children first: foreign_keys(1) refuses deleting a parent a child still names.
	for i := len(mirroredTables) - 1; i >= 0; i-- {
		table := mirroredTables[i]
		if _, ok := tables[table]; !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("apply snapshot: clear %s: %w", table, err)
		}
	}
	for _, table := range mirroredTables {
		data, ok := tables[table]
		if !ok {
			continue
		}
		if err := insertRows(ctx, tx, table, data, appliedAt); err != nil {
			return err
		}
	}
	return nil
}

// insertRows inserts one table's rows verbatim, ids and all: the leader's ids are the
// identity a rollback, a config version or a stack pointer names, so renumbering them
// would break every reference the row carries.
func insertRows(ctx context.Context, tx *sql.Tx, table string, data TableData, appliedAt int64) error {
	if len(data.Columns) == 0 {
		return fmt.Errorf("apply snapshot: table %s arrived with no columns", table)
	}
	if len(data.Columns) != len(data.Types) {
		return fmt.Errorf("apply snapshot: table %s arrived with %d columns and %d types", table, len(data.Columns), len(data.Types))
	}
	for _, col := range data.Columns {
		if !plainIdent(col) {
			return fmt.Errorf("apply snapshot: table %s has an unusable column name %q", table, col)
		}
	}
	stamp := -1
	for i, col := range data.Columns {
		if col == "mirrored_at" {
			// This node's own arrival time, never the leader's: a mirror that copies the
			// leader's stamp claims rows arrived before they did.
			stamp = i
		}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(data.Columns)), ",")
	stmt := `INSERT INTO ` + table + ` (` + strings.Join(data.Columns, ", ") + `) VALUES (` + placeholders + `)`
	for _, row := range data.Rows {
		if len(row) != len(data.Columns) {
			return fmt.Errorf("apply snapshot: table %s has a row with %d values for %d columns", table, len(row), len(data.Columns))
		}
		args := make([]any, len(row))
		for i, v := range row {
			if i == stamp {
				args[i] = appliedAt
				continue
			}
			coerced, err := mirrorValue(v, data.Types[i])
			if err != nil {
				return fmt.Errorf("apply snapshot: table %s column %s: %w", table, data.Columns[i], err)
			}
			args[i] = coerced
		}
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("apply snapshot: insert into %s: %w", table, err)
		}
	}
	return nil
}

// mirrorValue turns one decoded JSON cell back into the SQLite value it came from.
//
// The declared type is what makes a BLOB decodable: JSON has no byte string, so
// encoding/json writes one as base64 text, and without the column's type a
// base64-shaped TEXT and a BLOB would be indistinguishable. Numbers arrive as
// json.Number (the client decodes with UseNumber) and are converted to int64 unless
// they carry a fraction — an INTEGER column handed a float64 is a value SQLite would
// store as REAL.
func mirrorValue(v any, declaredType string) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		if isBlobType(declaredType) {
			decoded, err := base64.StdEncoding.DecodeString(x)
			if err != nil {
				return nil, fmt.Errorf("base64: %w", err)
			}
			return decoded, nil
		}
		return x, nil
	case []byte:
		return x, nil
	case int64:
		return x, nil
	case float64:
		return x, nil
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	case json.Number:
		raw := x.String()
		if !strings.ContainsAny(raw, ".eE") {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("integer %q: %w", raw, err)
			}
			return n, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, fmt.Errorf("number %q: %w", raw, err)
		}
		return f, nil
	}
	return nil, fmt.Errorf("unexpected value type %T", v)
}

// isBlobType reports whether a declared column type means bytes.
func isBlobType(declared string) bool {
	return strings.Contains(strings.ToUpper(declared), "BLOB")
}

// cell helpers: the legacy projection is derived from the document, so a value has to
// be read back out of a positional row without a typed Scan.

func columnIndex(cols []string, name string) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	return -1
}

func cellString(row []any, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	switch v := row[idx].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case int64:
		return strconv.FormatInt(v, 10)
	}
	return ""
}

func cellInt(row []any, idx int) int64 {
	if idx < 0 || idx >= len(row) {
		return 0
	}
	switch v := row[idx].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func cellBytes(row []any, idx int) []byte {
	if idx < 0 || idx >= len(row) {
		return nil
	}
	switch v := row[idx].(type) {
	case []byte:
		return v
	case string:
		return []byte(v)
	}
	return nil
}

// legacySettings, legacySecrets and legacyUsers are the pre-document projection,
// derived from the document rather than read a second time. They exist for a follower
// still running a binary that only understands `tables: {settings, secrets, users}`.
func legacySettings(tables map[string]TableData) []SnapshotSetting {
	out := []SnapshotSetting{}
	data, ok := tables["cluster_settings"]
	if !ok {
		return out
	}
	k, v := columnIndex(data.Columns, "key"), columnIndex(data.Columns, "value")
	for _, row := range data.Rows {
		out = append(out, SnapshotSetting{Key: cellString(row, k), Value: cellString(row, v)})
	}
	return out
}

func legacySecrets(tables map[string]TableData) []SnapshotSecret {
	out := []SnapshotSecret{}
	data, ok := tables["secrets"]
	if !ok {
		return out
	}
	id, scope, stack := columnIndex(data.Columns, "id"), columnIndex(data.Columns, "scope"), columnIndex(data.Columns, "stack")
	name, payload, hash := columnIndex(data.Columns, "name"), columnIndex(data.Columns, "payload"), columnIndex(data.Columns, "hash")
	created, rev := columnIndex(data.Columns, "created_at"), columnIndex(data.Columns, "swarm_rev")
	for _, row := range data.Rows {
		out = append(out, SnapshotSecret{
			ID:        cellInt(row, id),
			Scope:     cellString(row, scope),
			Stack:     cellString(row, stack),
			Name:      cellString(row, name),
			Payload:   cellBytes(row, payload),
			Hash:      cellString(row, hash),
			CreatedAt: cellInt(row, created),
			SwarmRev:  cellInt(row, rev),
		})
	}
	return out
}

func legacyUsers(tables map[string]TableData) []SnapshotUser {
	out := []SnapshotUser{}
	data, ok := tables["users"]
	if !ok {
		return out
	}
	id, name, tokenID := columnIndex(data.Columns, "id"), columnIndex(data.Columns, "name"), columnIndex(data.Columns, "token_id")
	hash, role, stack := columnIndex(data.Columns, "token_hash"), columnIndex(data.Columns, "role"), columnIndex(data.Columns, "stack")
	created := columnIndex(data.Columns, "created_at")
	for _, row := range data.Rows {
		out = append(out, SnapshotUser{
			ID:        cellInt(row, id),
			Name:      cellString(row, name),
			TokenID:   cellString(row, tokenID),
			TokenHash: cellString(row, hash),
			Role:      cellString(row, role),
			Stack:     cellString(row, stack),
			CreatedAt: cellInt(row, created),
		})
	}
	return out
}

// TableRowCounts counts the rows of every mirrored table on THIS node. It is what a
// verify report prints: a promotion is only as good as what the store actually holds, and
// "version 9" says nothing about whether the stacks came with it.
func (s *Store) TableRowCounts(ctx context.Context) (map[string]int, error) {
	out := make(map[string]int, len(mirroredTables))
	for _, table := range mirroredTables {
		var n int
		if err := s.conn().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		out[table] = n
	}
	return out, nil
}

// HasData reports whether this store holds any cluster state at all: a setting, a user, a
// stack or a secret. It is the difference between "a node that has not mirrored yet" and
// "a node with nothing to serve", and only the second one must never take leadership.
func (s *Store) HasData(ctx context.Context) (bool, error) {
	for _, table := range []string{"cluster_settings", "users", "stacks", "secrets", "configs"} {
		var n int
		if err := s.conn().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			return false, fmt.Errorf("count %s: %w", table, err)
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}
