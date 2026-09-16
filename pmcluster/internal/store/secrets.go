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

// SecretRow is a DB-backed secret. Payload holds the AES-GCM ciphertext
// (nonce || seal) — callers encrypt/decrypt via internal/credentials using
// ~/.pmcluster/.encryption_key. Hash is sha256(plaintext) so the value can
// be verified and displayed without ever being revealed.
type SecretRow struct {
	ID        int64
	Scope     string
	Stack     string
	Name      string
	Payload   []byte
	Hash      string
	CreatedAt int64
}

// ErrSecretNotFound is returned by secret getters/deleters when no row matches.
var ErrSecretNotFound = errors.New("secret not found")

// ErrSecretExists is returned by CreateSecret when the name is taken.
var ErrSecretExists = errors.New("secret already exists")

// SecretHash computes the sha256 hex fingerprint of a secret's plaintext
// value. Stored alongside the ciphertext so values can be verified and
// displayed without ever being revealed.
func SecretHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// CreateSecret inserts a new secret. Returns ErrSecretExists on name clash.
func (s *Store) CreateSecret(ctx context.Context, scope, stack, name string, payload []byte, hash string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets (scope, stack, name, payload, hash, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		scope, stack, name, payload, hash, time.Now().Unix(),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrSecretExists
		}
		return 0, fmt.Errorf("insert secret: %w", err)
	}
	return res.LastInsertId()
}

// GetSecret fetches a secret by name (including ciphertext payload).
// Returns ErrSecretNotFound when missing.
func (s *Store) GetSecret(ctx context.Context, name string) (*SecretRow, error) {
	var r SecretRow
	err := s.db.QueryRowContext(ctx,
		`SELECT id, scope, stack, name, payload, hash, created_at FROM secrets WHERE name = ?`, name,
	).Scan(&r.ID, &r.Scope, &r.Stack, &r.Name, &r.Payload, &r.Hash, &r.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSecretNotFound
		}
		return nil, fmt.Errorf("query secret: %w", err)
	}
	return &r, nil
}

// ListSecrets returns secrets filtered by scope and stack (empty values are
// wildcards) ordered by scope then stack then name. The ciphertext payload is
// deliberately excluded — callers wanting a value use GetSecret.
func (s *Store) ListSecrets(ctx context.Context, scope, stack string) ([]*SecretRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, scope, stack, name, hash, created_at FROM secrets
		 WHERE (?1 = '' OR scope = ?1) AND (?2 = '' OR stack = ?2)
		 ORDER BY scope, stack, name`, scope, stack)
	if err != nil {
		return nil, fmt.Errorf("query secrets: %w", err)
	}
	defer rows.Close()
	var out []*SecretRow
	for rows.Next() {
		var r SecretRow
		if err := rows.Scan(&r.ID, &r.Scope, &r.Stack, &r.Name, &r.Hash, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan secret: %w", err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// UpdateSecret replaces the ciphertext payload and hash of an existing secret,
// keeping its scope, stack, name and created_at. Returns ErrSecretNotFound
// when no row matched.
func (s *Store) UpdateSecret(ctx context.Context, name string, payload []byte, hash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE secrets SET payload = ?, hash = ? WHERE name = ?`, payload, hash, name)
	if err != nil {
		return fmt.Errorf("update secret: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrSecretNotFound
	}
	return nil
}

// DeleteSecret removes a secret. Returns ErrSecretNotFound when no row matched.
func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrSecretNotFound
	}
	return nil
}
