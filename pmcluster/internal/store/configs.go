package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ConfigRow is a DB-backed config value. Scope is "cluster" (cluster-level,
// applied by `cluster update`; Stack stays "") or "service" (belongs to one
// application stack, resolved at deploy time). Content is the editable value;
// Version records the pmcluster build that last wrote it; Hash is
// sha256(content) for change detection.
type ConfigRow struct {
	ID        int64
	Scope     string
	Stack     string
	Name      string
	Kind      string
	Content   string
	Version   string
	Hash      string
	CreatedAt int64
	UpdatedAt int64

	// RenderedContent holds the last post-substitution snapshot of this
	// config, written by every cluster update; RenderedAt is its unix
	// timestamp. Empty means no render has been recorded yet.
	RenderedContent string
	RenderedAt      int64
}

// ConfigVersionRow is one entry of a config's edit history. Content/Hash
// describe that point-in-time value, so a rollback can restore it exactly.
type ConfigVersionRow struct {
	ID        int64
	ConfigID  int64
	Content   string
	Hash      string
	CreatedAt int64
}

// ErrConfigNotFound is returned by config getters/deleters when no row matches.
var ErrConfigNotFound = errors.New("config not found")

// ErrConfigExists is returned by CreateConfig when the name is taken.
var ErrConfigExists = errors.New("config already exists")

// ErrConfigVersionNotFound is returned when a rollback references a version
// row that doesn't exist or belongs to another config.
var ErrConfigVersionNotFound = errors.New("config version not found")

// ConfigHash computes the sha256 hex fingerprint of config content. Shared
// by create/update so the hash column always matches the content bytes.
func ConfigHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// CreateConfig inserts a new config. Returns ErrConfigExists on name clash.
func (s *Store) CreateConfig(ctx context.Context, scope, stack, name, kind, content, version string) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO configs (scope, stack, name, kind, content, version, hash, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		scope, stack, name, kind, content, version, ConfigHash(content), now, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrConfigExists
		}
		return 0, fmt.Errorf("insert config: %w", err)
	}
	return res.LastInsertId()
}

// GetConfig fetches a config by name. Returns ErrConfigNotFound when missing.
func (s *Store) GetConfig(ctx context.Context, name string) (*ConfigRow, error) {
	var c ConfigRow
	err := s.db.QueryRowContext(ctx,
		`SELECT id, scope, stack, name, kind, content, version, hash, created_at, updated_at, rendered_content, rendered_at
		 FROM configs WHERE name = ?`, name,
	).Scan(&c.ID, &c.Scope, &c.Stack, &c.Name, &c.Kind, &c.Content,
		&c.Version, &c.Hash, &c.CreatedAt, &c.UpdatedAt, &c.RenderedContent, &c.RenderedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConfigNotFound
		}
		return nil, fmt.Errorf("query config: %w", err)
	}
	return &c, nil
}

// ListConfigs returns configs matching the given scope/stack filters (empty
// string = wildcard), ordered by scope then stack then name.
func (s *Store) ListConfigs(ctx context.Context, scope, stack string) ([]*ConfigRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, scope, stack, name, kind, content, version, hash, created_at, updated_at, rendered_content, rendered_at
		 FROM configs
		 WHERE (?1 = '' OR scope = ?1) AND (?2 = '' OR stack = ?2)
		 ORDER BY scope, stack, name`, scope, stack)
	if err != nil {
		return nil, fmt.Errorf("query configs: %w", err)
	}
	defer rows.Close()
	var out []*ConfigRow
	for rows.Next() {
		var c ConfigRow
		if err := rows.Scan(&c.ID, &c.Scope, &c.Stack, &c.Name, &c.Kind, &c.Content,
			&c.Version, &c.Hash, &c.CreatedAt, &c.UpdatedAt, &c.RenderedContent, &c.RenderedAt); err != nil {
			return nil, fmt.Errorf("scan config: %w", err)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// UpdateConfig replaces a config's content, preserving the previous value in
// config_versions so it can be rolled back. Returns the new hash.
// Returns ErrConfigNotFound when the config doesn't exist.
func (s *Store) UpdateConfig(ctx context.Context, name, newContent, version string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		id      int64
		content string
		hash    string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, content, hash FROM configs WHERE name = ?`, name,
	).Scan(&id, &content, &hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrConfigNotFound
		}
		return "", fmt.Errorf("query config: %w", err)
	}

	newHash := ConfigHash(newContent)
	if newHash == hash {

		return hash, nil
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO config_versions (config_id, content, hash, created_at) VALUES (?, ?, ?, ?)`,
		id, content, hash, time.Now().Unix(),
	); err != nil {
		return "", fmt.Errorf("insert config version: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE configs SET content = ?, version = ?, hash = ?, updated_at = ? WHERE id = ?`,
		newContent, version, newHash, time.Now().Unix(), id,
	); err != nil {
		return "", fmt.Errorf("update config: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit tx: %w", err)
	}
	return newHash, nil
}

// ListConfigVersions returns a config's edit history, newest first.
// Returns ErrConfigNotFound if the config doesn't exist.
func (s *Store) ListConfigVersions(ctx context.Context, name string) ([]*ConfigVersionRow, error) {
	var cfgID int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM configs WHERE name = ?`, name).Scan(&cfgID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConfigNotFound
		}
		return nil, fmt.Errorf("query config: %w", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, config_id, content, hash, created_at
		 FROM config_versions WHERE config_id = ? ORDER BY id DESC`, cfgID)
	if err != nil {
		return nil, fmt.Errorf("query config versions: %w", err)
	}
	defer rows.Close()
	var out []*ConfigVersionRow
	for rows.Next() {
		var v ConfigVersionRow
		if err := rows.Scan(&v.ID, &v.ConfigID, &v.Content, &v.Hash, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan config version: %w", err)
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

// RollbackConfig restores a config to a previous version (by version row id).
// The current value is pushed into config_versions first, so the rollback
// itself is reversible. Returns the restored hash.
func (s *Store) RollbackConfig(ctx context.Context, name string, versionID int64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		cfgID   int64
		content string
		hash    string
		version string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, content, hash, version FROM configs WHERE name = ?`, name,
	).Scan(&cfgID, &content, &hash, &version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrConfigNotFound
		}
		return "", fmt.Errorf("query config: %w", err)
	}

	var (
		oldContent string
		oldHash    string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT content, hash FROM config_versions WHERE id = ? AND config_id = ?`,
		versionID, cfgID,
	).Scan(&oldContent, &oldHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrConfigVersionNotFound
		}
		return "", fmt.Errorf("query config version: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO config_versions (config_id, content, hash, created_at) VALUES (?, ?, ?, ?)`,
		cfgID, content, hash, time.Now().Unix(),
	); err != nil {
		return "", fmt.Errorf("insert config version: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE configs SET content = ?, hash = ?, updated_at = ? WHERE id = ?`,
		oldContent, oldHash, time.Now().Unix(), cfgID,
	); err != nil {
		return "", fmt.Errorf("update config: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit tx: %w", err)
	}
	return oldHash, nil
}

// DeleteConfig removes a config and its version history (CASCADE).
// Returns ErrConfigNotFound when no row matched.
func (s *Store) DeleteConfig(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM configs WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete config: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrConfigNotFound
	}
	return nil
}

// SetRendered stamps a config with its latest post-substitution snapshot
// (rendered_content + rendered_at). Returns ErrConfigNotFound when the
// config doesn't exist.
func (s *Store) SetRendered(ctx context.Context, name, content string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE configs SET rendered_content = ?, rendered_at = ? WHERE name = ?`,
		content, time.Now().Unix(), name)
	if err != nil {
		return fmt.Errorf("set rendered config: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrConfigNotFound
	}
	return nil
}

// ListRenderedConfigs returns every config that has a rendered snapshot
// (rendered_content set), ordered by name.
func (s *Store) ListRenderedConfigs(ctx context.Context) ([]*ConfigRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, scope, stack, name, kind, content, version, hash, created_at, updated_at, rendered_content, rendered_at
		 FROM configs WHERE rendered_content != '' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("query rendered configs: %w", err)
	}
	defer rows.Close()
	var out []*ConfigRow
	for rows.Next() {
		var c ConfigRow
		if err := rows.Scan(&c.ID, &c.Scope, &c.Stack, &c.Name, &c.Kind, &c.Content,
			&c.Version, &c.Hash, &c.CreatedAt, &c.UpdatedAt, &c.RenderedContent, &c.RenderedAt); err != nil {
			return nil, fmt.Errorf("scan rendered config: %w", err)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}
