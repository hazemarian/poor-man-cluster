package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Registry holds Docker registry credentials for one host. Used by
// `pmcluster registry add` to remember what was logged in (so we can
// re-apply on a fresh manager AND show via `pmcluster registry list`).
//
// PasswordCiphertext is AES-GCM-encrypted via the credentials.Cipher.
type Registry struct {
	Host               string
	Username           string
	PasswordCiphertext []byte
	CreatedAt          int64
}

// ErrRegistryNotFound is returned by Registry when no row matches.
var ErrRegistryNotFound = errors.New("registry not found")

// ErrRegistryExists is returned by CreateRegistry on UNIQUE collision.
var ErrRegistryExists = errors.New("registry already exists")

// CreateRegistry inserts a new registry host. Errors with ErrRegistryExists
// if the host is already configured (use UpdateRegistry to rotate creds).
func (s *Store) CreateRegistry(ctx context.Context, r *Registry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO registries (host, username, password_ciphertext, created_at)
		 VALUES (?, ?, ?, ?)`,
		r.Host, r.Username, r.PasswordCiphertext, time.Now().Unix(),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrRegistryExists
		}
		return fmt.Errorf("insert registry: %w", err)
	}
	return nil
}

// UpdateRegistry replaces the username + password for an existing host.
// Returns ErrRegistryNotFound if the host isn't configured.
func (s *Store) UpdateRegistry(ctx context.Context, r *Registry) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE registries SET username = ?, password_ciphertext = ? WHERE host = ?`,
		r.Username, r.PasswordCiphertext, r.Host,
	)
	if err != nil {
		return fmt.Errorf("update registry: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrRegistryNotFound
	}
	return nil
}

// Registry fetches one registry by host.
func (s *Store) Registry(ctx context.Context, host string) (*Registry, error) {
	var r Registry
	err := s.db.QueryRowContext(ctx,
		`SELECT host, username, password_ciphertext, created_at FROM registries WHERE host = ?`, host,
	).Scan(&r.Host, &r.Username, &r.PasswordCiphertext, &r.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRegistryNotFound
		}
		return nil, fmt.Errorf("query registry: %w", err)
	}
	return &r, nil
}

// ListRegistries returns all configured registries ordered by host.
func (s *Store) ListRegistries(ctx context.Context) ([]*Registry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT host, username, password_ciphertext, created_at FROM registries ORDER BY host`,
	)
	if err != nil {
		return nil, fmt.Errorf("query registries: %w", err)
	}
	defer rows.Close()
	var out []*Registry
	for rows.Next() {
		var r Registry
		if err := rows.Scan(&r.Host, &r.Username, &r.PasswordCiphertext, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan registry: %w", err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// DeleteRegistry removes a registry. Returns ErrRegistryNotFound if missing.
func (s *Store) DeleteRegistry(ctx context.Context, host string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM registries WHERE host = ?`, host)
	if err != nil {
		return fmt.Errorf("delete registry: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrRegistryNotFound
	}
	return nil
}
