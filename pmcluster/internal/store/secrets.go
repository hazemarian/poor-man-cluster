package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/errs"
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
	// SwarmRev is how many times the value has been rotated. Docker swarm
	// secrets are immutable and cannot be removed while in use, so every
	// rotation mirrors the value into a NEW swarm secret named by
	// SwarmSecretName (content-addressed — see below).
	SwarmRev int64
}

// SwarmSecretName returns the Docker swarm secret name backing a DB secret.
// Names are CONTENT-ADDRESSED: the 8-hex-char sha256 prefix of the value is
// appended, so the same value always yields the same name (an unchanged
// secret is reused as-is — no number is ever added for an unchanged render)
// and a real value change mints a brand-new name. The DB row is the source
// of truth for "which is the latest": translation looks up the row, takes
// its Hash, and derives the exact swarm secret name to reference. The
// container mount path stays /run/secrets/<name> regardless.
func SwarmSecretName(name, hash string) string {
	if len(hash) < 8 {
		return name
	}
	return name + "_" + hash[:8]
}

// ErrSecretNotFound is returned by secret getters/deleters when no row matches.
// It aliases the canonical sentinel in internal/errs (also aliased by the
// secrets domain), so errors.Is works for either name.
var ErrSecretNotFound = errs.ErrSecretNotFound

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

// Secret fetches a secret by name (including ciphertext payload).
// Returns ErrSecretNotFound when missing.
func (s *Store) Secret(ctx context.Context, name string) (*SecretRow, error) {
	var r SecretRow
	err := s.db.QueryRowContext(ctx,
		`SELECT id, scope, stack, name, payload, hash, created_at, swarm_rev FROM secrets WHERE name = ?`, name,
	).Scan(&r.ID, &r.Scope, &r.Stack, &r.Name, &r.Payload, &r.Hash, &r.CreatedAt, &r.SwarmRev)
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
// deliberately excluded — callers wanting a value use Secret.
//
// A stack filter also matches unattached service-scope rows (stack = ”):
// the DSL resolves secrets(name) by name only, so an unattached service
// secret is usable by any stack and must stay visible on every stack's page.
func (s *Store) ListSecrets(ctx context.Context, scope, stack string) ([]*SecretRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, scope, stack, name, hash, created_at, swarm_rev FROM secrets
		 WHERE (?1 = '' OR scope = ?1) AND (?2 = '' OR stack = ?2 OR (?2 != '' AND ?1 = 'service' AND stack = ''))
		 ORDER BY scope, stack, name`, scope, stack)
	if err != nil {
		return nil, fmt.Errorf("query secrets: %w", err)
	}
	defer rows.Close()
	var out []*SecretRow
	for rows.Next() {
		var r SecretRow
		if err := rows.Scan(&r.ID, &r.Scope, &r.Stack, &r.Name, &r.Hash, &r.CreatedAt, &r.SwarmRev); err != nil {
			return nil, fmt.Errorf("scan secret: %w", err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// UpdateSecret replaces the ciphertext payload and hash of an existing secret
// and bumps its swarm rotation counter (SwarmRev +1) so the CLI can mirror
// the new value into a fresh versioned swarm secret without touching the
// immutable in-use one. Keeps scope, stack, name and created_at.
// Returns ErrSecretNotFound when no row matched.
func (s *Store) UpdateSecret(ctx context.Context, name string, payload []byte, hash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE secrets SET payload = ?, hash = ?, swarm_rev = swarm_rev + 1 WHERE name = ?`, payload, hash, name)
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

// UpdateSecretStack retags a secret row's scope and stack. This is how a row
// created unattached (stack "") gets bound to a stack — or moved between them —
// from the console or CLI, without touching its payload. Returns
// ErrSecretNotFound when no row matched.
func (s *Store) UpdateSecretStack(ctx context.Context, name, scope, stack string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE secrets SET scope = ?, stack = ? WHERE name = ?`, scope, stack, name)
	if err != nil {
		return fmt.Errorf("update secret stack: %w", err)
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
