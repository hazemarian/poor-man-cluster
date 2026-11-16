package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// cluster_settings is one of the tables a follower mirrors (see Store.Snapshot),
// so every write here shares a transaction with the cluster-version bump.

// ErrSettingNotFound is returned by Setting when no row matches.
var ErrSettingNotFound = errors.New("setting not found")

// Setting reads a cluster_settings value. Returns ErrSettingNotFound when
// the key has never been written.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.conn().QueryRowContext(ctx,
		`SELECT value FROM cluster_settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSettingNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get setting %s: %w", key, err)
	}
	return value, nil
}

// SetSetting upserts a cluster_settings value, bumping the cluster version in the
// same transaction when the value actually changes.
//
// Writing the SAME value is a no-op that does not bump. That is not an
// optimisation: the reconcile loop persists settings it recomputes on every pass,
// and a bump per pass would invalidate every follower's ETag forever, so the
// snapshot endpoint would answer 200 and a full body on every poll while nothing
// had changed.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	return s.inTxBump(ctx, func(tx *sql.Tx) (bool, error) {
		var old string
		err := tx.QueryRowContext(ctx, `SELECT value FROM cluster_settings WHERE key = ?`, key).Scan(&old)
		switch {
		case err == nil && old == value:
			return false, nil
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return false, fmt.Errorf("read setting %s: %w", key, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cluster_settings (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			key, value); err != nil {
			return false, fmt.Errorf("set setting %s: %w", key, err)
		}
		return true, nil
	})
}

// DeleteSetting removes a cluster_settings row, bumping the cluster version only
// when a row was actually removed. Absent keys are not an error — the naming
// migration deletes old keys unconditionally so it stays idempotent — and they
// must not bump either, or every daemon start would publish a new version for
// nothing.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	return s.inTxBump(ctx, func(tx *sql.Tx) (bool, error) {
		res, err := tx.ExecContext(ctx, `DELETE FROM cluster_settings WHERE key = ?`, key)
		if err != nil {
			return false, fmt.Errorf("delete setting %s: %w", key, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("rows affected deleting setting %s: %w", key, err)
		}
		return n > 0, nil
	})
}

// SettingDefault reads a setting, returning fallback (not an error) when
// the key is absent. A genuine read failure (e.g. a closed/backing store) is
// logged at warn — with the key — and still returns the fallback, so optional
// settings never break the caller; only sql.ErrNoRows (translated to
// ErrSettingNotFound by Setting) is silently defaulted.
func (s *Store) SettingDefault(ctx context.Context, key, fallback string) string {
	v, err := s.Setting(ctx, key)
	if err == nil {
		return v
	}
	if !errors.Is(err, ErrSettingNotFound) {
		s.Log.Warn().Err(err).Str("key", key).Msg("store — get setting failed; using default")
	}
	return fallback
}
