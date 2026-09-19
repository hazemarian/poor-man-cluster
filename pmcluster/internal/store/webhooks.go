package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WebhookSource is one configured CI/automation source allowed to POST to
// /webhook/{source}. The secret is stored encrypted (AES-GCM via the
// credentials.Cipher); pmcluster recomputes HMAC over each request body
// to verify the signature header.
type WebhookSource struct {
	Source           string
	SecretCiphertext []byte
	Description      sql.NullString
	CreatedAt        int64
	LastUsedAt       sql.NullInt64
}

// ErrWebhookSourceNotFound is returned by GetWebhookSource when no row matches.
var ErrWebhookSourceNotFound = errors.New("webhook source not found")

// CreateWebhookSource inserts a new source with its encrypted shared secret.
// Returns ErrWebhookSourceExists if the source name is already taken.
func (s *Store) CreateWebhookSource(ctx context.Context, source, description string, secretCiphertext []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_sources (source, secret_ciphertext, description, created_at)
		 VALUES (?, ?, ?, ?)`,
		source, secretCiphertext, nullableString(description, description != ""), time.Now().Unix(),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrWebhookSourceExists
		}
		return fmt.Errorf("insert webhook source: %w", err)
	}
	return nil
}

// ErrWebhookSourceExists is returned by CreateWebhookSource on UNIQUE collision.
var ErrWebhookSourceExists = errors.New("webhook source already exists")

// GetWebhookSource fetches a source by name. Used by the webhook handler to
// look up the secret for HMAC verification.
func (s *Store) GetWebhookSource(ctx context.Context, source string) (*WebhookSource, error) {
	var w WebhookSource
	err := s.db.QueryRowContext(ctx,
		`SELECT source, secret_ciphertext, description, created_at, last_used_at
		 FROM webhook_sources WHERE source = ?`, source,
	).Scan(&w.Source, &w.SecretCiphertext, &w.Description, &w.CreatedAt, &w.LastUsedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrWebhookSourceNotFound
		}
		return nil, fmt.Errorf("query webhook source: %w", err)
	}
	return &w, nil
}

// ListWebhookSources returns all configured sources ordered by name.
func (s *Store) ListWebhookSources(ctx context.Context) ([]*WebhookSource, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT source, secret_ciphertext, description, created_at, last_used_at
		 FROM webhook_sources ORDER BY source`,
	)
	if err != nil {
		return nil, fmt.Errorf("query webhook sources: %w", err)
	}
	defer rows.Close()
	var out []*WebhookSource
	for rows.Next() {
		var w WebhookSource
		if err := rows.Scan(&w.Source, &w.SecretCiphertext, &w.Description, &w.CreatedAt, &w.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scan webhook source: %w", err)
		}
		out = append(out, &w)
	}
	return out, rows.Err()
}

// DeleteWebhookSource removes a configured source. Returns ErrWebhookSourceNotFound
// if no row matched (so the caller can surface a friendly message).
func (s *Store) DeleteWebhookSource(ctx context.Context, source string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhook_sources WHERE source = ?`, source)
	if err != nil {
		return fmt.Errorf("delete webhook source: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrWebhookSourceNotFound
	}
	return nil
}

// MarkWebhookSourceUsed updates last_used_at to now. Best-effort —
// callers should NOT fail the webhook on this error.
func (s *Store) MarkWebhookSourceUsed(ctx context.Context, source string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webhook_sources SET last_used_at = ? WHERE source = ?`,
		time.Now().Unix(), source,
	)
	return err
}

// WebhookDelivery is one recorded delivery against a webhook source:
// the outcome status plus deploy provenance when the request made it to deploy.
type WebhookDelivery struct {
	ID        int64
	Source    string
	Status    string // accepted | unauthorized | bad_request | server_error
	StackName string
	Revision  int64
	RepoURL   string
	File      string
	Error     string
	CreatedAt int64
}

// RecordWebhookDelivery persists one delivery outcome. Best-effort —
// recording must never fail the webhook request itself.
func (s *Store) RecordWebhookDelivery(ctx context.Context, d *WebhookDelivery) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_deliveries (source, status, stack_name, revision, repo_url, file, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Source, d.Status, d.StackName, d.Revision, d.RepoURL, d.File, d.Error, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("insert webhook delivery: %w", err)
	}
	return nil
}

// ListWebhookDeliveries returns the most recent deliveries for a source,
// newest first, limited to `limit` rows (0 = no limit).
func (s *Store) ListWebhookDeliveries(ctx context.Context, source string, limit int) ([]*WebhookDelivery, error) {
	q := `SELECT id, source, status, stack_name, revision, repo_url, file, error, created_at
		  FROM webhook_deliveries WHERE source = ? ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q, source)
	if err != nil {
		return nil, fmt.Errorf("query webhook deliveries: %w", err)
	}
	defer rows.Close()
	var out []*WebhookDelivery
	for rows.Next() {
		var d WebhookDelivery
		if err := rows.Scan(&d.ID, &d.Source, &d.Status, &d.StackName, &d.Revision, &d.RepoURL, &d.File, &d.Error, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan webhook delivery: %w", err)
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}
