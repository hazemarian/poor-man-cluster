package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ManagedCredential is the persisted form of a credential pmcluster owns
// for one of the bundled components (Traefik admin, OpenObserve, edge console).
//
// PasswordCiphertext is opaque — callers decrypt via the credentials package.
type ManagedCredential struct {
	Name               string
	Kind               string
	Username           string
	PasswordCiphertext []byte
	SwarmSecretName    string
	CreatedAt          int64
	RotatedAt          sql.NullInt64
}

// ErrCredentialNotFound is returned by GetCredential when no row matches.
var ErrCredentialNotFound = errors.New("credential not found")

// GetCredential fetches a credential row by name.
// Returns ErrCredentialNotFound if missing.
func (s *Store) GetCredential(ctx context.Context, name string) (*ManagedCredential, error) {
	var c ManagedCredential
	err := s.db.QueryRowContext(ctx,
		`SELECT name, kind, username, password_ciphertext, swarm_secret_name, created_at, rotated_at
		 FROM managed_credentials WHERE name = ?`, name,
	).Scan(&c.Name, &c.Kind, &c.Username, &c.PasswordCiphertext,
		&c.SwarmSecretName, &c.CreatedAt, &c.RotatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCredentialNotFound
		}
		return nil, fmt.Errorf("query credential: %w", err)
	}
	return &c, nil
}

// InsertCredential stores a brand-new credential. Fails if name already
// exists (UNIQUE collision); callers should `GetCredential` first to
// implement the "preserve existing" policy on cluster up.
func (s *Store) InsertCredential(ctx context.Context, c *ManagedCredential) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO managed_credentials
		   (name, kind, username, password_ciphertext, swarm_secret_name, created_at, rotated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Kind, c.Username, c.PasswordCiphertext,
		c.SwarmSecretName, time.Now().Unix(), nil,
	)
	if err != nil {
		return fmt.Errorf("insert credential: %w", err)
	}
	return nil
}

// RotateCredential replaces the ciphertext for an existing credential and
// stamps rotated_at. Returns ErrCredentialNotFound if the row doesn't exist.
func (s *Store) RotateCredential(ctx context.Context, name string, newCiphertext []byte) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE managed_credentials
		 SET password_ciphertext = ?, rotated_at = ?
		 WHERE name = ?`,
		newCiphertext, time.Now().Unix(), name,
	)
	if err != nil {
		return fmt.Errorf("update credential: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrCredentialNotFound
	}
	return nil
}

// UpdateCredentialUsername changes the username for an existing credential.
// Returns ErrCredentialNotFound if the row doesn't exist.
func (s *Store) UpdateCredentialUsername(ctx context.Context, name string, newUsername string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE managed_credentials SET username = ? WHERE name = ?`,
		newUsername, name,
	)
	if err != nil {
		return fmt.Errorf("update credential username: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrCredentialNotFound
	}
	return nil
}

// ListCredentials returns all credentials, sorted by name. Used by
// `pmcluster credentials list`.
func (s *Store) ListCredentials(ctx context.Context) ([]*ManagedCredential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, kind, username, password_ciphertext, swarm_secret_name, created_at, rotated_at
		 FROM managed_credentials ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("query credentials: %w", err)
	}
	defer rows.Close()
	var out []*ManagedCredential
	for rows.Next() {
		var c ManagedCredential
		if err := rows.Scan(&c.Name, &c.Kind, &c.Username, &c.PasswordCiphertext,
			&c.SwarmSecretName, &c.CreatedAt, &c.RotatedAt); err != nil {
			return nil, fmt.Errorf("scan credential: %w", err)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}
