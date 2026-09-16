package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSiteCertsCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	notBefore := now.Add(-24 * time.Hour)
	notAfter := now.Add(90 * 24 * time.Hour)

	row := SiteCertRow{
		Domain:     "example.com",
		CertSecret: "cert_v012",
		KeySecret:  "key_v012",
		NotBefore:  notBefore,
		NotAfter:   notAfter,
		SANs:       []string{"example.com", "www.example.com"},
		CertHash:   "aa11",
		KeyHash:    "bb22",
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.PutSiteCert(ctx, row); err != nil {
		t.Fatalf("PutSiteCert: %v", err)
	}
	got, err := s.GetSiteCert(ctx, "example.com")
	if err != nil {
		t.Fatalf("GetSiteCert: %v", err)
	}
	if got.Domain != row.Domain {
		t.Errorf("domain = %q, want %q", got.Domain, row.Domain)
	}
	if got.CertSecret != "cert_v012" || got.KeySecret != "key_v012" {
		t.Errorf("secrets = %q/%q, want cert_v012/key_v012", got.CertSecret, got.KeySecret)
	}
	if !got.NotAfter.Equal(notAfter) {
		t.Errorf("NotAfter = %v, want %v", got.NotAfter, notAfter)
	}
	if !got.NotBefore.Equal(notBefore) {
		t.Errorf("NotBefore = %v, want %v", got.NotBefore, notBefore)
	}
	if len(got.SANs) != 2 || got.SANs[0] != "example.com" || got.SANs[1] != "www.example.com" {
		t.Errorf("SANs = %v, want [example.com www.example.com]", got.SANs)
	}
	if got.CertHash != "aa11" || got.KeyHash != "bb22" {
		t.Errorf("hashes = %s/%s, want aa11/bb22", got.CertHash, got.KeyHash)
	}

	later := now.Add(time.Hour)
	row2 := row
	row2.CertSecret = "cert_v013"
	row2.KeySecret = "key_v013"
	row2.CertHash = "cc33"
	row2.UpdatedAt = later
	if err := s.PutSiteCert(ctx, row2); err != nil {
		t.Fatalf("PutSiteCert (update): %v", err)
	}
	got2, err := s.GetSiteCert(ctx, "example.com")
	if err != nil {
		t.Fatalf("GetSiteCert (after update): %v", err)
	}
	if got2.CertSecret != "cert_v013" {
		t.Errorf("CertSecret after update = %q, want cert_v013", got2.CertSecret)
	}
	if !got2.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt changed: got %v, want %v", got2.CreatedAt, now)
	}
	if !got2.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt = %v, want %v", got2.UpdatedAt, later)
	}

	if err := s.DeleteSiteCert(ctx, "example.com"); err != nil {
		t.Fatalf("DeleteSiteCert: %v", err)
	}
	if _, err := s.GetSiteCert(ctx, "example.com"); !errors.Is(err, ErrSiteCertNotFound) {
		t.Errorf("GetSiteCert after delete: err = %v, want ErrSiteCertNotFound", err)
	}

	if err := s.DeleteSiteCert(ctx, "example.com"); err != nil {
		t.Errorf("DeleteSiteCert (missing): %v", err)
	}

	if _, err := s.GetSiteCert(ctx, "other.com"); !errors.Is(err, ErrSiteCertNotFound) {
		t.Errorf("GetSiteCert (never stored): err = %v, want ErrSiteCertNotFound", err)
	}
}
