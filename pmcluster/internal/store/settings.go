package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrSettingNotFound is returned by GetSetting when no row matches.
var ErrSettingNotFound = errors.New("setting not found")

// GetSetting reads a cluster_settings value. Returns ErrSettingNotFound when
// the key has never been written.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM cluster_settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSettingNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get setting %s: %w", key, err)
	}
	return value, nil
}

// SetSetting upserts a cluster_settings value.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO cluster_settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value); err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

// GetSettingDefault reads a setting, returning fallback (not an error) when
// the key is absent. Convenient for optional values.
func (s *Store) GetSettingDefault(ctx context.Context, key, fallback string) string {
	v, err := s.GetSetting(ctx, key)
	if err != nil {
		return fallback
	}
	return v
}
