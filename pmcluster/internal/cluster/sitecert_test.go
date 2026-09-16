package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// genCert mints a self-signed ECDSA P-256 cert whose SANs cover host.
func genCert(t *testing.T, host string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	kder, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
	return certPEM, keyPEM
}

// TestImportSiteCertMetadata_OnceOnly verifies the idempotent "migration of the
// existing SSL": the first import seeds the row, later runs are no-ops, and an
// unparseable pair is skipped silently (best-effort metadata only).
func TestImportSiteCertMetadata_OnceOnly(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "seed.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	certPEM, keyPEM := genCert(t, "test.example.com")
	certPath := writeTempFile(t, dir, "cert.pem", []byte(certPEM))
	keyPath := writeTempFile(t, dir, "key.pem", []byte(keyPEM))

	if err := importSiteCertMetadata(ctx, s, certPath, keyPath, "test.example.com", "cert_v001", "key_v001"); err != nil {
		t.Fatalf("first import: %v", err)
	}
	row, err := s.GetSiteCert(ctx, "test.example.com")
	if err != nil {
		t.Fatalf("get after import: %v", err)
	}
	if row.CertSecret != "cert_v001" || row.KeySecret != "key_v001" {
		t.Errorf("secrets not recorded: cert=%q key=%q", row.CertSecret, row.KeySecret)
	}
	if row.CertHash != store.ConfigHash(certPEM) || row.KeyHash != store.ConfigHash(keyPEM) {
		t.Errorf("hashes not recorded: cert=%q key=%q", row.CertHash, row.KeyHash)
	}
	if row.Domain != "test.example.com" {
		t.Errorf("domain = %q", row.Domain)
	}
	if row.NotAfter.IsZero() || !row.NotAfter.After(time.Now()) {
		t.Errorf("NotAfter should be populated and in the future, got %v", row.NotAfter)
	}

	// Second import (different secret names) must be a no-op — row unchanged.
	if err := importSiteCertMetadata(ctx, s, certPath, keyPath, "test.example.com", "cert_v002", "key_v002"); err != nil {
		t.Fatalf("second import: %v", err)
	}
	row2, err := s.GetSiteCert(ctx, "test.example.com")
	if err != nil {
		t.Fatalf("get after second import: %v", err)
	}
	if row2.CertSecret != "cert_v001" {
		t.Errorf("second import should be a no-op, cert secret changed to %q", row2.CertSecret)
	}
}

// TestImportSiteCertMetadata_SkipsUnparseable verifies best-effort behavior:
// a pair that fails ParseAndCheck (placeholder bytes, wrong domain) leaves the
// row absent with no error.
func TestImportSiteCertMetadata_SkipsUnparseable(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "seed.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))
	if err := importSiteCertMetadata(ctx, s, certPath, keyPath, "test.example.com", "cert_v001", "key_v001"); err != nil {
		t.Fatalf("import of unparseable pair should not error, got: %v", err)
	}
	if _, err := s.GetSiteCert(ctx, "test.example.com"); !errors.Is(err, store.ErrSiteCertNotFound) {
		t.Fatalf("expected ErrSiteCertNotFound (unparseable pair must not seed), got: %v", err)
	}

	// Nil inputs also no-op cleanly.
	if err := importSiteCertMetadata(ctx, s, "", "", "test.example.com", "", ""); err != nil {
		t.Fatalf("empty inputs should no-op, got: %v", err)
	}
}

// TestApplySiteCert_EndToEnd exercises the full site-cert apply path: the pair
// is validated for the persisted domain, written as the local source of truth
// under <configDir>/site/, the TLS state is re-pointed, Traefik is refreshed
// through the Update pipeline, and the metadata is stored.
func TestApplySiteCert_EndToEnd(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)

	ctx := context.Background()
	certPEM, keyPEM := genCert(t, "test.example.com")
	scdeps := SiteCertDeps{Store: deps.Store, Cipher: deps.Cipher, Docker: deps.Docker, Deployer: deps.Deployer, Provisioner: deps.Provisioner}
	row, err := ApplySiteCert(ctx, scdeps, cfgDir, "v0.3.0", "test.example.com", certPEM, keyPEM)
	if err != nil {
		t.Fatalf("ApplySiteCert: %v", err)
	}

	// Local source-of-truth files written.
	certPath := filepath.Join(SiteCertDir(cfgDir), "cert.pem")
	keyPath := filepath.Join(SiteCertDir(cfgDir), "key.pem")
	for _, p := range []string{certPath, keyPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s to exist: %v", p, err)
		}
	}
	gotCert, _ := os.ReadFile(certPath)
	gotKey, _ := os.ReadFile(keyPath)
	if string(gotCert) != certPEM || string(gotKey) != keyPEM {
		t.Error("local site cert files do not match the uploaded PEMs")
	}

	// TLS state re-pointed at the local copies.
	state, err := loadTLSSettings(ctx, deps.Store)
	if err != nil {
		t.Fatalf("load tls settings: %v", err)
	}
	if state.CertPath != certPath || state.KeyPath != keyPath || state.Mode != "cert" {
		t.Errorf("tls state not re-pointed: %+v", state)
	}

	// Metadata stored.
	if row.Domain != "test.example.com" {
		t.Errorf("row domain = %q", row.Domain)
	}
	if row.CertSecret == "" || row.KeySecret == "" {
		t.Errorf("secrets should be materialized by the update pipeline, got %q/%q", row.CertSecret, row.KeySecret)
	}
	if row.CertHash != store.ConfigHash(certPEM) {
		t.Errorf("cert hash mismatch")
	}

	// Idempotence: applying the SAME cert again must still succeed (content-
	// aware pipeline reuses the secret versions) and keep the same row secrets.
	row2, err := ApplySiteCert(ctx, scdeps, cfgDir, "v0.3.0", "test.example.com", certPEM, keyPEM)
	if err != nil {
		t.Fatalf("second ApplySiteCert: %v", err)
	}
	if row2.CertSecret != row.CertSecret || row2.KeySecret != row.KeySecret {
		t.Errorf("idempotent re-apply should reuse secret versions: before %q/%q after %q/%q",
			row.CertSecret, row.KeySecret, row2.CertSecret, row2.KeySecret)
	}
}

// TestApplySiteCert_RejectsWrongDomain verifies the coverage check: a cert that
// does not cover the cluster's own domain must be rejected before anything is
// written or refreshed.
func TestApplySiteCert_RejectsWrongDomain(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)

	ctx := context.Background()
	certPEM, keyPEM := genCert(t, "other.example.net")
	scdeps := SiteCertDeps{Store: deps.Store, Cipher: deps.Cipher, Docker: deps.Docker, Deployer: deps.Deployer, Provisioner: deps.Provisioner}
	_, err := ApplySiteCert(ctx, scdeps, cfgDir, "v0.3.0", "test.example.com", certPEM, keyPEM)
	if err == nil {
		t.Fatal("expected an error: cert for other.example.net must not be accepted for test.example.com")
	}
	if !strings.Contains(err.Error(), "does not cover") {
		t.Errorf("error should explain the coverage failure, got: %v", err)
	}
}

// TestGetSiteCert verifies the getter semantics.
func TestGetSiteCert(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "seed.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if _, err := GetSiteCert(ctx, s, "test.example.com"); !errors.Is(err, store.ErrSiteCertNotFound) {
		t.Fatalf("expected ErrSiteCertNotFound before any apply, got: %v", err)
	}
	if _, err := GetSiteCert(ctx, nil, "test.example.com"); !errors.Is(err, store.ErrSiteCertNotFound) {
		t.Fatalf("nil store should report ErrSiteCertNotFound, got: %v", err)
	}

	// After seeding via the import path, the getter returns the row.
	certPEM, keyPEM := genCert(t, "test.example.com")
	certPath := writeTempFile(t, dir, "cert.pem", []byte(certPEM))
	keyPath := writeTempFile(t, dir, "key.pem", []byte(keyPEM))
	if err := importSiteCertMetadata(ctx, s, certPath, keyPath, "test.example.com", "cert_v001", "key_v001"); err != nil {
		t.Fatalf("import: %v", err)
	}
	row, err := GetSiteCert(ctx, s, "test.example.com")
	if err != nil {
		t.Fatalf("get after import: %v", err)
	}
	if row.Domain != "test.example.com" || row.CertSecret != "cert_v001" {
		t.Errorf("unexpected row: %+v", row)
	}
}

// TestWarnSiteCertExpiry verifies the renewal warning printed by cluster
// up/update: silent without a row, a warning when expiring within the window,
// and a stronger message once expired.
func TestWarnSiteCertExpiry(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "seed.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	domain := "test.example.com"
	now := time.Now().UTC()

	var buf strings.Builder
	warnSiteCertExpiry(ctx, s, domain, &buf)
	if buf.Len() != 0 {
		t.Fatalf("expected silence without a row; got: %q", buf.String())
	}

	seed := func(notAfter time.Time) {
		buf.Reset()
		if err := s.PutSiteCert(ctx, store.SiteCertRow{
			Domain: domain, CertSecret: "cert_v001", KeySecret: "key_v001",
			NotBefore: now, NotAfter: notAfter, SANs: []string{domain},
			CertHash: "a", KeyHash: "b", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	seed(now.AddDate(0, 0, 10))
	warnSiteCertExpiry(ctx, s, domain, &buf)
	if !strings.Contains(buf.String(), "expires in") || !strings.Contains(buf.String(), "tls site set") {
		t.Errorf("expiring-soon warning missing; got: %q", buf.String())
	}

	seed(now.AddDate(0, 0, -3))
	warnSiteCertExpiry(ctx, s, domain, &buf)
	if !strings.Contains(buf.String(), "expired 3 days ago") {
		t.Errorf("expired warning missing; got: %q", buf.String())
	}

	seed(now.AddDate(1, 0, 0))
	warnSiteCertExpiry(ctx, s, domain, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silence for a fresh cert; got: %q", buf.String())
	}

	warnSiteCertExpiry(ctx, nil, domain, &buf)
	if buf.Len() != 0 {
		t.Errorf("nil store should be silent; got: %q", buf.String())
	}
}
