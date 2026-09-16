package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrSiteCertNotFound is returned by GetSiteCert when no row matches the
// domain (no site certificate has been uploaded yet).
var ErrSiteCertNotFound = errors.New("site cert not found")

// SiteCertRow is the persisted metadata for one TLS certificate — either the
// cluster's own (main-domain) certificate or a per-host (bring-your-own)
// certificate. The PEM bytes themselves live in versioned Swarm secrets
// (CertSecret/KeySecret) — this table only records where they are and when
// they expire, so the console can display and warn about each cert.
type SiteCertRow struct {
	Domain     string
	CertSecret string
	KeySecret  string
	NotBefore  time.Time
	NotAfter   time.Time
	SANs       []string
	CertHash   string
	KeyHash    string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// GetSiteCert returns the stored site-cert metadata for the cluster domain.
// Returns ErrSiteCertNotFound when none has been uploaded yet.
func (s *Store) GetSiteCert(ctx context.Context, domain string) (SiteCertRow, error) {
	var r SiteCertRow
	var sans string
	var nb, na, created, updated int64
	err := s.db.QueryRowContext(ctx,
		`SELECT domain, cert_secret, key_secret, not_before, not_after, sans,
		        cert_hash, key_hash, created_at, updated_at
		   FROM site_certs WHERE domain = ?`, domain).
		Scan(&r.Domain, &r.CertSecret, &r.KeySecret, &nb, &na, &sans,
			&r.CertHash, &r.KeyHash, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return SiteCertRow{}, ErrSiteCertNotFound
	}
	if err != nil {
		return SiteCertRow{}, fmt.Errorf("get site cert %s: %w", domain, err)
	}
	r.NotBefore = time.Unix(nb, 0).UTC()
	r.NotAfter = time.Unix(na, 0).UTC()
	r.SANs = splitCSV(sans)
	r.CreatedAt = time.Unix(created, 0).UTC()
	r.UpdatedAt = time.Unix(updated, 0).UTC()
	return r, nil
}

// ListSiteCerts returns every stored cert metadata row (the cluster's own
// domain AND every per-host bring-your-own cert — all live in this one
// table), sorted by domain.
func (s *Store) ListSiteCerts(ctx context.Context) ([]SiteCertRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT domain, cert_secret, key_secret, not_before, not_after, sans,
		        cert_hash, key_hash, created_at, updated_at
		   FROM site_certs ORDER BY domain`)
	if err != nil {
		return nil, fmt.Errorf("list site certs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SiteCertRow
	for rows.Next() {
		var r SiteCertRow
		var sans string
		var nb, na, created, updated int64
		if err := rows.Scan(&r.Domain, &r.CertSecret, &r.KeySecret, &nb, &na, &sans,
			&r.CertHash, &r.KeyHash, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan site cert row: %w", err)
		}
		r.NotBefore = time.Unix(nb, 0).UTC()
		r.NotAfter = time.Unix(na, 0).UTC()
		r.SANs = splitCSV(sans)
		r.CreatedAt = time.Unix(created, 0).UTC()
		r.UpdatedAt = time.Unix(updated, 0).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate site certs: %w", err)
	}
	return out, nil
}

// PutSiteCert upserts the site-cert metadata for the cluster domain.
// The row's CreatedAt/UpdatedAt are used as-is (the caller stamps them);
// on update, created_at is preserved and only updated_at moves forward.
func (s *Store) PutSiteCert(ctx context.Context, r SiteCertRow) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO site_certs (domain, cert_secret, key_secret, not_before,
		        not_after, sans, cert_hash, key_hash, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(domain) DO UPDATE SET
		   cert_secret = excluded.cert_secret,
		   key_secret  = excluded.key_secret,
		   not_before  = excluded.not_before,
		   not_after   = excluded.not_after,
		   sans        = excluded.sans,
		   cert_hash   = excluded.cert_hash,
		   key_hash    = excluded.key_hash,
		   updated_at  = excluded.updated_at`,
		r.Domain, r.CertSecret, r.KeySecret, r.NotBefore.Unix(), r.NotAfter.Unix(),
		joinCSV(r.SANs), r.CertHash, r.KeyHash, r.CreatedAt.Unix(), r.UpdatedAt.Unix()); err != nil {
		return fmt.Errorf("put site cert %s: %w", r.Domain, err)
	}
	return nil
}

// DeleteSiteCert removes the stored site-cert metadata for the domain.
// No error when nothing was stored.
func (s *Store) DeleteSiteCert(ctx context.Context, domain string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM site_certs WHERE domain = ?`, domain); err != nil {
		return fmt.Errorf("delete site cert %s: %w", domain, err)
	}
	return nil
}

func joinCSV(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	cur := ""
	for _, ch := range s {
		if ch == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(ch)
	}
	out = append(out, cur)
	return out
}
