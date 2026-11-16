package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The cluster-state version: one row, bumped in the same transaction as every
// write to a table a follower mirrors. See migrations/0030_cluster_version.sql
// for why it is one row and why the timestamp lives here rather than in the
// handler.
//
// # What the version is, and is not
//
// It identifies a CONTENT state, not a point in time. A follower compares
// (leader, version) for equality — the pair is the ETag — and must not read a
// numerically lower version as "older": a leader that adopts a control-plane
// snapshot, or a store restored from a kit, can answer with a version it has
// used before, for content that differs. Treating the counter as a clock is the
// bug this comment exists to prevent.
const (
	clusterVersionSQL = `SELECT version, generated_at FROM cluster_version WHERE id = 1`
	bumpVersionSQL    = `UPDATE cluster_version SET version = version + 1, generated_at = ? WHERE id = 1`
)

// ClusterVersion returns the current version and the unix second it was stamped.
func (s *Store) ClusterVersion(ctx context.Context) (version, generatedAt int64, err error) {
	err = s.conn().QueryRowContext(ctx, clusterVersionSQL).Scan(&version, &generatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// The migration seeds the row, so this cannot happen to a store this
		// binary opened. Answering 0 would be worse than failing: a follower
		// would treat it as a version and mirror on it forever.
		return 0, 0, fmt.Errorf("cluster version row missing (migration 0030 did not seed it)")
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read cluster version: %w", err)
	}
	return version, generatedAt, nil
}

// inTxBump runs fn in one transaction and bumps the cluster version in that SAME
// transaction when fn reports that the mirrored content changed.
//
// fn returning changed=false is not a failure: it is how a no-op write stays a
// no-op. Reconcile rewrites the same settings every pass, and a bump for a write
// that changed nothing would invalidate every follower's ETag on every pass —
// 304 would be dead weight and every poll would rewrite rows for no state change.
//
// The bump is not best-effort. A write that commits without its bump is invisible
// to followers until some unrelated write advances the counter, so a failing bump
// rolls the write back: either both landed or neither did.
func (s *Store) inTxBump(ctx context.Context, fn func(tx *sql.Tx) (changed bool, err error)) error {
	tx, err := s.conn().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	changed, err := fn(tx)
	if err != nil {
		return err
	}
	if !changed {
		// Commit anyway: "nothing to bump" is not "nothing to keep". A delete that
		// removed rows outside the mirrored set (a stack and its configs, say) still
		// has to land — returning early without the commit rolled the work back while
		// answering success, which is exactly the silent no-op class this file is
		// meant to prevent.
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		return nil
	}
	res, err := tx.ExecContext(ctx, bumpVersionSQL, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("bump cluster version: %w", err)
	}
	// RowsAffected is the guard against the silent no-op class: the UPDATE is
	// WHERE id = 1, so 0 rows means the seeded row is gone and the version would
	// never move again while every write kept succeeding.
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected by cluster version bump: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("cluster version bump touched %d rows, want exactly 1 (the seeded row is missing)", n)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
