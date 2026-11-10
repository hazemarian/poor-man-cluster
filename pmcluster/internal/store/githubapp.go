package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GitHubApp holds the cluster's GitHub App registration. It is a singleton:
// at most one row exists, with id = 1.
//
// PrivateKeyCiphertext is AES-256-GCM encrypted via the credentials.Cipher.
// The installation token derived from this key is NEVER stored — it lives in
// memory in internal/githubapp and has a one-hour lifetime.
type GitHubApp struct {
	AppID                int64
	InstallationID       int64
	PrivateKeyCiphertext []byte
	ProbeRepo            string
	ProbePackage         string
	// KeyFingerprint identifies the stored key without revealing it (SHA-256 of
	// the public half, `ssh-keygen -lf` shape). Empty on a row written before
	// the column existed; the daemon recomputes it from the key when it reads
	// one of those.
	KeyFingerprint string
	// LastProbe* record the last registration change or Test that minted a token
	// and read the configured targets. LastProbeAt is Unix seconds; 0 means the
	// pairing has never been proven since this column existed.
	LastProbeOK     bool
	LastProbeDetail string
	LastProbeAt     int64
	CreatedAt       int64
	UpdatedAt       int64
}

// ErrGitHubAppNotConfigured is returned when no app credentials are stored.
var ErrGitHubAppNotConfigured = errors.New("github app not configured")

// GitHubApp reads the singleton row.
func (s *Store) GitHubApp(ctx context.Context) (*GitHubApp, error) {
	var a GitHubApp
	err := s.conn().QueryRowContext(ctx,
		`SELECT app_id, installation_id, private_key_ciphertext, probe_repo, probe_package,
		        key_fingerprint, last_probe_ok, last_probe_detail, last_probe_at,
		        created_at, updated_at
		   FROM github_app WHERE id = 1`,
	).Scan(&a.AppID, &a.InstallationID, &a.PrivateKeyCiphertext, &a.ProbeRepo, &a.ProbePackage,
		&a.KeyFingerprint, &a.LastProbeOK, &a.LastProbeDetail, &a.LastProbeAt,
		&a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGitHubAppNotConfigured
		}
		return nil, fmt.Errorf("query github app: %w", err)
	}
	return &a, nil
}

// SetGitHubApp upserts the singleton row. created_at is preserved on update.
func (s *Store) SetGitHubApp(ctx context.Context, a *GitHubApp) error {
	now := time.Now().Unix()
	_, err := s.conn().ExecContext(ctx,
		`INSERT INTO github_app (id, app_id, installation_id, private_key_ciphertext, probe_repo, probe_package,
		                         key_fingerprint, last_probe_ok, last_probe_detail, last_probe_at,
		                         created_at, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   app_id = excluded.app_id,
		   installation_id = excluded.installation_id,
		   private_key_ciphertext = excluded.private_key_ciphertext,
		   probe_repo = excluded.probe_repo,
		   probe_package = excluded.probe_package,
		   key_fingerprint = excluded.key_fingerprint,
		   last_probe_ok = excluded.last_probe_ok,
		   last_probe_detail = excluded.last_probe_detail,
		   last_probe_at = excluded.last_probe_at,
		   updated_at = excluded.updated_at`,
		a.AppID, a.InstallationID, a.PrivateKeyCiphertext, a.ProbeRepo, a.ProbePackage,
		a.KeyFingerprint, a.LastProbeOK, a.LastProbeDetail, a.LastProbeAt, now, now,
	)
	if err != nil {
		return fmt.Errorf("set github app: %w", err)
	}
	return nil
}

// DeleteGitHubApp removes the singleton row. Absent is not an error: clearing
// an unconfigured app is a no-op, which keeps `pmcluster github app clear`
// idempotent.
func (s *Store) DeleteGitHubApp(ctx context.Context) error {
	if _, err := s.conn().ExecContext(ctx, `DELETE FROM github_app WHERE id = 1`); err != nil {
		return fmt.Errorf("delete github app: %w", err)
	}
	return nil
}

// GitHubBrokerSecret is one repository's HMAC shared secret for the token
// broker endpoint.
type GitHubBrokerSecret struct {
	Repo             string
	SecretCiphertext []byte
	Description      string
	CreatedAt        int64
	LastUsedAt       int64
}

// ErrGitHubBrokerSecretNotFound is returned when a repo has no broker secret.
var ErrGitHubBrokerSecretNotFound = errors.New("github broker secret not found")

// ErrGitHubBrokerSecretExists is returned when a repo already has a secret.
var ErrGitHubBrokerSecretExists = errors.New("github broker secret already exists")

// CreateGitHubBrokerSecret inserts one repo's secret.
func (s *Store) CreateGitHubBrokerSecret(ctx context.Context, sec *GitHubBrokerSecret) error {
	_, err := s.conn().ExecContext(ctx,
		`INSERT INTO github_broker_secrets (repo, secret_ciphertext, description, created_at)
		 VALUES (?, ?, ?, ?)`,
		sec.Repo, sec.SecretCiphertext, sec.Description, time.Now().Unix(),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrGitHubBrokerSecretExists
		}
		return fmt.Errorf("insert github broker secret: %w", err)
	}
	return nil
}

// UpdateGitHubBrokerSecret rotates an existing repo secret.
func (s *Store) UpdateGitHubBrokerSecret(ctx context.Context, sec *GitHubBrokerSecret) error {
	res, err := s.conn().ExecContext(ctx,
		`UPDATE github_broker_secrets SET secret_ciphertext = ?, description = ? WHERE repo = ?`,
		sec.SecretCiphertext, sec.Description, sec.Repo)
	if err != nil {
		return fmt.Errorf("update github broker secret: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrGitHubBrokerSecretNotFound
	}
	return nil
}

// GitHubBrokerSecret reads one repo's secret.
func (s *Store) GitHubBrokerSecret(ctx context.Context, repo string) (*GitHubBrokerSecret, error) {
	var sec GitHubBrokerSecret
	var lastUsed sql.NullInt64
	err := s.conn().QueryRowContext(ctx,
		`SELECT repo, secret_ciphertext, description, created_at, last_used_at
		   FROM github_broker_secrets WHERE repo = ?`, repo,
	).Scan(&sec.Repo, &sec.SecretCiphertext, &sec.Description, &sec.CreatedAt, &lastUsed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGitHubBrokerSecretNotFound
		}
		return nil, fmt.Errorf("query github broker secret: %w", err)
	}
	if lastUsed.Valid {
		sec.LastUsedAt = lastUsed.Int64
	}
	return &sec, nil
}

// ListGitHubBrokerSecrets returns every repo secret ordered by repo name.
func (s *Store) ListGitHubBrokerSecrets(ctx context.Context) ([]*GitHubBrokerSecret, error) {
	rows, err := s.conn().QueryContext(ctx,
		`SELECT repo, secret_ciphertext, description, created_at, last_used_at
		   FROM github_broker_secrets ORDER BY repo`)
	if err != nil {
		return nil, fmt.Errorf("query github broker secrets: %w", err)
	}
	defer rows.Close()
	var out []*GitHubBrokerSecret
	for rows.Next() {
		var sec GitHubBrokerSecret
		var lastUsed sql.NullInt64
		if err := rows.Scan(&sec.Repo, &sec.SecretCiphertext, &sec.Description, &sec.CreatedAt, &lastUsed); err != nil {
			return nil, fmt.Errorf("scan github broker secret: %w", err)
		}
		if lastUsed.Valid {
			sec.LastUsedAt = lastUsed.Int64
		}
		out = append(out, &sec)
	}
	return out, rows.Err()
}

// DeleteGitHubBrokerSecret removes one repo secret.
func (s *Store) DeleteGitHubBrokerSecret(ctx context.Context, repo string) error {
	res, err := s.conn().ExecContext(ctx, `DELETE FROM github_broker_secrets WHERE repo = ?`, repo)
	if err != nil {
		return fmt.Errorf("delete github broker secret: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrGitHubBrokerSecretNotFound
	}
	return nil
}

// MarkGitHubBrokerSecretUsed records the last successful mint (best-effort).
func (s *Store) MarkGitHubBrokerSecretUsed(ctx context.Context, repo string) error {
	if _, err := s.conn().ExecContext(ctx,
		`UPDATE github_broker_secrets SET last_used_at = ? WHERE repo = ?`, time.Now().Unix(), repo); err != nil {
		return fmt.Errorf("mark github broker secret used: %w", err)
	}
	return nil
}
