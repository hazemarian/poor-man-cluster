package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrRenderedConfigNotFound is returned when no render is stored for a name.
var ErrRenderedConfigNotFound = errors.New("rendered config not found")

// RenderedRow is one snapshotted render of a platform config: the post-substitution
// YAML that was sent to the Swarm by the last cluster update.
type RenderedRow struct {
	ID        int64
	Name      string
	Content   string
	Hash      string
	CreatedAt int64
	UpdatedAt int64
}

// PutRenderedConfig upserts the rendered YAML for a platform config name. The
// first insert stamps created_at; later writes keep it and move only
// content/hash/updated_at.
func (s *Store) PutRenderedConfig(ctx context.Context, name, content string) error {
	now := time.Now().Unix()
	_, err := s.DB().ExecContext(ctx, `
		INSERT INTO rendered_configs (name, content, hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			content = excluded.content,
			hash = excluded.hash,
			updated_at = excluded.updated_at
	`, name, content, ConfigHash(content), now, now)
	return err
}

// ListRenderedConfigs returns every stored render ordered by name.
func (s *Store) ListRenderedConfigs(ctx context.Context) ([]*RenderedRow, error) {
	rows, err := s.DB().QueryContext(ctx, `
		SELECT id, name, content, hash, created_at, updated_at
		FROM rendered_configs
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*RenderedRow
	for rows.Next() {
		var r RenderedRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Content, &r.Hash, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// GetRenderedConfig returns the stored render for a single name.
func (s *Store) GetRenderedConfig(ctx context.Context, name string) (*RenderedRow, error) {
	var r RenderedRow
	err := s.DB().QueryRowContext(ctx, `
		SELECT id, name, content, hash, created_at, updated_at
		FROM rendered_configs
		WHERE name = ?
	`, name).Scan(&r.ID, &r.Name, &r.Content, &r.Hash, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRenderedConfigNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}
