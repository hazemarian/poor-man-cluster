package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrNeverSynced is returned by SyncState on a node that has never applied a
// snapshot. It is not an error condition: a leader, a worker and a fresh follower
// all answer it, and each read surface says "no age yet" rather than inventing one.
var ErrNeverSynced = errors.New("this node has never applied a cluster snapshot")

// SyncState is the last snapshot a follower applied: which leader's state, at which
// version, when the leader stamped it, and when this node wrote it.
//
// The two timestamps are not interchangeable. GeneratedAt came from the leader and
// is provenance; AppliedAt is this node's clock and is the only sound basis for an
// age. Subtracting a remote stamp from a local clock is how a fresh report reads as
// stale forever.
type SyncState struct {
	Leader      string
	Version     int64
	GeneratedAt int64
	AppliedAt   int64
}

// SyncState reads the mirror's state. Returns ErrNeverSynced when this node has
// never applied a snapshot.
func (s *Store) SyncState(ctx context.Context) (*SyncState, error) {
	var st SyncState
	err := s.conn().QueryRowContext(ctx,
		`SELECT leader, version, generated_at, applied_at FROM sync_state WHERE id = 1`).
		Scan(&st.Leader, &st.Version, &st.GeneratedAt, &st.AppliedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNeverSynced
	}
	if err != nil {
		return nil, fmt.Errorf("read sync state: %w", err)
	}
	return &st, nil
}

// ApplySnapshot replaces this node's mirrored tables with snap, in ONE transaction:
// upsert every row, delete every row the snapshot does not carry, then record the
// version. leader is the hostname the response named (the ETag this node sends back
// must name the pair it applied, so an anonymous apply is refused), and appliedAt is
// this node's clock (unix seconds), stamped onto every row it writes and onto
// sync_state.
//
// A full replacement, not a delta: a row absent from the snapshot is deleted here,
// which a delta could not express without tombstones. That is also why the endpoint
// always sends every table as an array — an empty table means "delete them all", and
// the call must be able to tell that apart from "the leader did not send it".
//
// The upsert/delete split is per table because the natural keys differ:
//
//   - cluster_settings: key is the primary key, so ON CONFLICT(key) is a real upsert.
//   - secrets: name is UNIQUE, same shape.
//   - users: a row has TWO unique keys (id and name, the latter also the display
//     identity), and no local authority — every user row on a follower came from the
//     leader. Upserting there invites a conflict between the two keys (a name that
//     arrives under a new id fails on the UNIQUE index and stalls the mirror
//     forever). So the mirrored users table is REPLACED: delete, then insert the
//     leader's rows verbatim, ids and all. Same end state, no conflict surface.
//
// last_used_at is deliberately not part of the snapshot, so every mirrored user row
// carries 0: a follower has never seen that token used, and saying otherwise would
// be a claim about traffic it did not serve.
func (s *Store) ApplySnapshot(ctx context.Context, snap *Snapshot, leader string, appliedAt int64) error {
	if snap == nil {
		return errors.New("apply snapshot: nil snapshot")
	}
	if strings.TrimSpace(leader) == "" {
		// Without the leader the recorded state cannot produce the ETag the next poll
		// sends, so this node would re-download the same version forever while looking
		// synced. Refuse instead of recording a half-identity.
		return errors.New("apply snapshot: no leader named (the response must say whose state this is)")
	}
	tx, err := s.conn().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("apply snapshot: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := applySettings(ctx, tx, snap.Settings, appliedAt); err != nil {
		return err
	}
	if err := applySecrets(ctx, tx, snap.Secrets, appliedAt); err != nil {
		return err
	}
	if err := applyUsers(ctx, tx, snap.Users, appliedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_state (id, leader, version, generated_at, applied_at)
		 VALUES (1, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   leader = excluded.leader,
		   version = excluded.version,
		   generated_at = excluded.generated_at,
		   applied_at = excluded.applied_at`,
		leader, snap.Version, snap.GeneratedAt, appliedAt); err != nil {
		return fmt.Errorf("apply snapshot: record sync state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("apply snapshot: commit: %w", err)
	}
	return nil
}

func applySettings(ctx context.Context, tx *sql.Tx, rows []SnapshotSetting, appliedAt int64) error {
	keys := make([]any, 0, len(rows))
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cluster_settings (key, value, mirrored_at) VALUES (?, ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, mirrored_at = excluded.mirrored_at`,
			r.Key, r.Value, appliedAt); err != nil {
			return fmt.Errorf("apply snapshot: upsert setting %s: %w", r.Key, err)
		}
		keys = append(keys, r.Key)
	}
	return deleteAbsent(ctx, tx, "cluster_settings", "key", keys)
}

func applySecrets(ctx context.Context, tx *sql.Tx, rows []SnapshotSecret, appliedAt int64) error {
	names := make([]any, 0, len(rows))
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO secrets (id, scope, stack, name, payload, hash, created_at, swarm_rev, mirrored_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(name) DO UPDATE SET
			   scope = excluded.scope, stack = excluded.stack, payload = excluded.payload,
			   hash = excluded.hash, created_at = excluded.created_at,
			   swarm_rev = excluded.swarm_rev, mirrored_at = excluded.mirrored_at`,
			r.ID, r.Scope, r.Stack, r.Name, r.Payload, r.Hash, r.CreatedAt, r.SwarmRev, appliedAt); err != nil {
			return fmt.Errorf("apply snapshot: upsert secret %s: %w", r.Name, err)
		}
		names = append(names, r.Name)
	}
	return deleteAbsent(ctx, tx, "secrets", "name", names)
}

// applyUsers replaces the mirrored users table. See ApplySnapshot for why this one
// table is not an upsert.
func applyUsers(ctx context.Context, tx *sql.Tx, rows []SnapshotUser, appliedAt int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM users`); err != nil {
		return fmt.Errorf("apply snapshot: clear users: %w", err)
	}
	for _, r := range rows {
		// token_id is nullable in the schema and UNIQUE when set: a legacy row has
		// no token id, and inserting "" for several of them collides on the index.
		// NULL is the only faithful spelling of "this row has none".
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users (id, name, token_id, token_hash, created_at, last_used_at, stack, role, mirrored_at)
			 VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)`,
			r.ID, r.Name, nullIfEmpty(r.TokenID), r.TokenHash, r.CreatedAt, r.Stack, r.Role, appliedAt); err != nil {
			return fmt.Errorf("apply snapshot: insert user %s: %w", r.Name, err)
		}
	}
	return nil
}

// deleteAbsent removes every row of table whose key column is not in keys. An
// empty snapshot means the leader has no rows, so everything goes — this is the
// half of the apply that an upsert-only mirror gets wrong, and a deleted setting
// would otherwise live on every follower forever.
func deleteAbsent(ctx context.Context, tx *sql.Tx, table, column string, keys []any) error {
	if len(keys) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("apply snapshot: clear %s: %w", table, err)
		}
		return nil
	}
	// The identifiers are constants in this package; the placeholders are generated
	// from the row count, so no value is ever interpolated into the statement.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+table+` WHERE `+column+` NOT IN (`+placeholders+`)`, keys...); err != nil {
		return fmt.Errorf("apply snapshot: delete absent rows of %s: %w", table, err)
	}
	return nil
}

// nullIfEmpty maps the domain's "" onto SQL NULL, for columns where "" is a real
// value that a UNIQUE index would treat as a duplicate.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
