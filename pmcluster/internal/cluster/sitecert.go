package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster/tlscerts"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// siteCertDir is the subdirectory of the config dir where the cluster's own
// (main-domain) TLS cert/key are kept as the stable local source of truth.
// The PEM bytes also live in versioned Swarm secrets (cert_vN/key_vN); this
// local copy is what `cluster update` re-materializes from, so a cert uploaded
// through the console/CLI/API is never lost when the node reboots.
const siteCertDir = "site"

// SiteCertDeps bundles the collaborators a site-cert apply/refresh needs. It
// mirrors HostCertsDeps — the site-cert flow reuses the full Update pipeline.
type SiteCertDeps struct {
	Store       *store.Store
	Cipher      *credentials.Cipher
	Docker      docker.Client
	Deployer    StackDeployer
	Provisioner *OpenObserveProvisioner
}

// SiteCertDir returns <configDir>/site — the local home of the cluster's own
// TLS cert/key PEM files.
func SiteCertDir(configDir string) string {
	return filepath.Join(configDir, siteCertDir)
}

// PersistedDomain returns the cluster's own domain from persisted settings,
// or "" when nothing has been persisted yet (never an error). Useful for
// callers (daemon API, CLI) that need the domain to look up / target the
// site certificate.
func PersistedDomain(ctx context.Context, st *store.Store) string {
	if st == nil {
		return ""
	}
	return st.GetSettingDefault(ctx, settingDomain, "")
}

// EnsureSiteCertDir creates the site-cert directory (0755) if missing.
func EnsureSiteCertDir(configDir string) error {
	return os.MkdirAll(SiteCertDir(configDir), 0o755)
}

// ApplySiteCert validates certPEM/keyPEM for the cluster's own domain, writes
// them as the local source of truth under <configDir>/site/, re-points the
// persisted TLS state at those files, refreshes Traefik via the Update
// pipeline (materializing cert_vN/key_vN + re-rendering the dynamic config),
// and persists the certificate metadata in the store for expiry monitoring.
//
// It is the single implementation behind the CLI (`pmcluster tls site`) and
// the daemon API (PUT /api/tls/site). Requires the cluster to be up.
func ApplySiteCert(ctx context.Context, deps SiteCertDeps, configDir, version, domain, certPEM, keyPEM string) (*store.SiteCertRow, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("apply site cert requires a store (run `pmcluster init` + `pmcluster cluster up` first)")
	}
	if domain == "" {
		domain = deps.Store.GetSettingDefault(ctx, settingDomain, "")
		if domain == "" {
			return nil, fmt.Errorf("no persisted domain found — run `cluster up` before managing the site certificate")
		}
	}

	// Validate the pair and that the leaf covers the cluster's own domain
	// (CN or SAN), then extract metadata for storage + expiry monitoring.
	info, err := tlscerts.ParseAndCheck(certPEM, keyPEM, domain)
	if err != nil {
		return nil, fmt.Errorf("validate site certificate: %w", err)
	}

	// Stable local copy: <configDir>/site/cert.pem + key.pem (0600). Written
	// key-first, atomic rename per file, mirroring the per-host cert manager.
	if err := EnsureSiteCertDir(configDir); err != nil {
		return nil, fmt.Errorf("ensure site cert dir: %w", err)
	}
	certPath := filepath.Join(SiteCertDir(configDir), "cert.pem")
	keyPath := filepath.Join(SiteCertDir(configDir), "key.pem")
	if err := writeSiteCertFile(keyPath, keyPEM); err != nil {
		return nil, err
	}
	if err := writeSiteCertFile(certPath, certPEM); err != nil {
		return nil, err
	}

	// Re-point the persisted TLS state at the local copies (mode stays "cert";
	// an ACME-mode cluster switches to operator certs — allowed via this API,
	// which is explicit about the intent, unlike a bare `cluster up`).
	state := tlsState{Mode: "cert", CertPath: certPath, KeyPath: keyPath}
	if err := state.save(ctx, deps.Store); err != nil {
		return nil, err
	}

	// Refresh Traefik through the full Update pipeline: content-aware secret
	// materialization (cert_vN/key_vN), dynamic-config re-render, infra
	// re-deploy only when the content moved.
	res, err := Update(ctx, UpdateDeps{
		Store:       deps.Store,
		Cipher:      deps.Cipher,
		Docker:      deps.Docker,
		Deployer:    deps.Deployer,
		Provisioner: deps.Provisioner,
		Stdout:      io.Discard,
	}, UpdateInput{ConfigDir: configDir, Version: version})
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	row := store.SiteCertRow{
		Domain:     domain,
		CertSecret: res.CertSecret,
		KeySecret:  res.KeySecret,
		NotBefore:  info.NotBefore,
		NotAfter:   info.NotAfter,
		SANs:       info.SANs,
		CertHash:   store.ConfigHash(certPEM),
		KeyHash:    store.ConfigHash(keyPEM),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := deps.Store.PutSiteCert(ctx, row); err != nil {
		return nil, fmt.Errorf("store site certificate metadata: %w", err)
	}
	return &row, nil
}

// GetSiteCert returns the stored metadata for the cluster's own domain, or
// store.ErrSiteCertNotFound when nothing has been imported/uploaded yet.
func GetSiteCert(ctx context.Context, st *store.Store, domain string) (*store.SiteCertRow, error) {
	if st == nil {
		return nil, store.ErrSiteCertNotFound
	}
	row, err := st.GetSiteCert(ctx, domain)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// importSiteCertMetadata seeds the site_certs DB row from the currently
// applied TLS cert/key files when the row is absent — the idempotent
// "migration of the existing SSL" step. It is called from the Update pipeline
// after the cert/key secrets are materialized, so any `cluster up`/`update`
// on an upgraded binary imports the pre-existing certificate exactly once.
func importSiteCertMetadata(ctx context.Context, st *store.Store, certPath, keyPath, domain, certSecret, keySecret string) error {
	if st == nil || certPath == "" || keyPath == "" || domain == "" || certSecret == "" || keySecret == "" {
		return nil
	}
	if _, err := st.GetSiteCert(ctx, domain); err == nil {
		return nil // already imported
	} else if !errors.Is(err, store.ErrSiteCertNotFound) {
		return err
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("read site cert for import: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read site key for import: %w", err)
	}
	info, err := tlscerts.ParseAndCheck(string(certPEM), string(keyPEM), domain)
	if err != nil {
		// Best-effort import: the metadata is auxiliary (expiry monitoring).
		// The cert/key secrets were already materialized by the caller, so a
		// pair that doesn't parse (or doesn't cover the domain) just leaves
		// the DB row absent — the operator can upload a proper cert later.
		return nil
	}
	now := time.Now().UTC()
	return st.PutSiteCert(ctx, store.SiteCertRow{
		Domain:     domain,
		CertSecret: certSecret,
		KeySecret:  keySecret,
		NotBefore:  info.NotBefore,
		NotAfter:   info.NotAfter,
		SANs:       info.SANs,
		CertHash:   store.ConfigHash(string(certPEM)),
		KeyHash:    store.ConfigHash(string(keyPEM)),
		CreatedAt:  now,
		UpdatedAt:  now,
	})
}

// siteCertExpiryWarnDays is how close to expiry (in days) a site certificate
// must be before `cluster up`/`cluster update` print a renewal warning.
const siteCertExpiryWarnDays = 30

// warnSiteCertExpiry prints a renewal warning to out when the recorded site
// certificate is expired or within siteCertExpiryWarnDays of expiry. Best
// effort: missing metadata, an absent row or a nil store are silently ignored
// (ACME-mode clusters have no row and renew automatically).
func warnSiteCertExpiry(ctx context.Context, st *store.Store, domain string, out io.Writer) {
	if st == nil || domain == "" || out == nil {
		return
	}
	row, err := st.GetSiteCert(ctx, domain)
	if err != nil {
		return
	}
	days := int(time.Until(row.NotAfter).Hours() / 24)
	switch {
	case days < 0:
		fmt.Fprintf(out, "⚠ Site certificate for %s expired %d days ago (%s) — renew it with `pmcluster tls site set`\n",
			domain, -days, row.NotAfter.Format(time.RFC3339))
	case days <= siteCertExpiryWarnDays:
		fmt.Fprintf(out, "⚠ Site certificate for %s expires in %d days (%s) — renew it with `pmcluster tls site set`\n",
			domain, days, row.NotAfter.Format(time.RFC3339))
	}
}

// writeSiteCertFile writes PEM data to path with 0600 perms via an atomic
// temp-file rename (so a crash mid-write can't leave a half-written cert).
func writeSiteCertFile(path, data string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".site-cert-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // no-op after successful rename
	if _, err := tmp.WriteString(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename onto %s: %w", path, err)
	}
	return nil
}
