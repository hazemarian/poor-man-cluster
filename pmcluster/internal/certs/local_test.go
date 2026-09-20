package certs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Local is exercised against a real store. ApplyHostCert / RemoveHostCert /
// SiteCert need full cluster wiring (cluster.ApplyCert → Docker + Swarm), so
// only the read-side (GetSiteCert / List / MainDomain) and the row mapping
// helpers are covered here.

func seedSiteCert(t *testing.T, st *store.Store, domain string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.PutSiteCert(context.Background(), store.SiteCertRow{
		Domain:     domain,
		CertSecret: "cert_v1",
		KeySecret:  "key_v1",
		NotBefore:  now.Add(-time.Hour),
		NotAfter:   now.Add(90 * 24 * time.Hour),
		SANs:       []string{domain, "www." + domain},
		CertHash:   "aa",
		KeyHash:    "bb",
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("PutSiteCert: %v", err)
	}
}

func TestLocalGetSiteCert(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	seedSiteCert(t, st, "example.com")

	svc := &Local{Store: st}
	got, err := svc.GetSiteCert(ctx, "example.com")
	if err != nil {
		t.Fatalf("GetSiteCert: %v", err)
	}
	if got.Domain != "example.com" || got.CertSecret != "cert_v1" || got.KeySecret != "key_v1" {
		t.Errorf("GetSiteCert = %+v, want domain/example.com cert_v1/key_v1", got)
	}
	if got.CertHash != "aa" || got.KeyHash != "bb" {
		t.Errorf("hashes = %s/%s, want aa/bb", got.CertHash, got.KeyHash)
	}
	if len(got.SANs) != 2 {
		t.Errorf("SANs = %v, want 2 entries", got.SANs)
	}

	if _, err := svc.GetSiteCert(ctx, "missing.com"); !errors.Is(err, store.ErrSiteCertNotFound) {
		t.Errorf("GetSiteCert(missing) = %v, want ErrSiteCertNotFound", err)
	}
}

func TestLocalList(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	seedSiteCert(t, st, "example.com")
	seedSiteCert(t, st, "host.example.com")

	svc := &Local{Store: st}
	rows, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("List = %d certs, want 2", len(rows))
	}
	if rows[0].Domain != "example.com" || rows[1].Domain != "host.example.com" {
		t.Errorf("List order = [%s %s], want sorted by domain", rows[0].Domain, rows[1].Domain)
	}
}

func TestLocalMainDomain(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)

	svc := &Local{Store: st}
	got, err := svc.MainDomain(ctx)
	if err != nil {
		t.Fatalf("MainDomain: %v", err)
	}
	if got != "" {
		t.Errorf("MainDomain (unset) = %q, want empty", got)
	}

	if err := st.SetSetting(ctx, "domain", "example.com"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	got, err = svc.MainDomain(ctx)
	if err != nil {
		t.Fatalf("MainDomain: %v", err)
	}
	if got != "example.com" {
		t.Errorf("MainDomain = %q, want example.com", got)
	}
}

func TestRowModelMapping(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	row := store.SiteCertRow{
		Domain:     "example.com",
		CertSecret: "cert_v1",
		KeySecret:  "key_v1",
		NotBefore:  now.Add(-time.Hour),
		NotAfter:   now.Add(24 * time.Hour),
		SANs:       []string{"example.com"},
		CertHash:   "aa",
		KeyHash:    "bb",
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	m := rowModel(&row)
	if m.Domain != row.Domain || m.CertSecret != row.CertSecret || m.KeySecret != row.KeySecret {
		t.Errorf("rowModel = %+v, want fields preserved", m)
	}
	if !m.NotBefore.Equal(row.NotBefore) || !m.NotAfter.Equal(row.NotAfter) {
		t.Errorf("rowModel times = %v/%v, want %v/%v", m.NotBefore, m.NotAfter, row.NotBefore, row.NotAfter)
	}

	rows := rowModels([]store.SiteCertRow{row, row})
	if len(rows) != 2 {
		t.Fatalf("rowModels = %d certs, want 2", len(rows))
	}
}
