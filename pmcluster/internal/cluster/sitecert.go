package cluster

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster/tlscerts"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
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
	Store    *store.Store
	Cipher   *credentials.Cipher
	Docker   runtime.Client
	Deployer StackDeployer
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

// ApplyCert validates certPEM/keyPEM for the target domain and uploads it —
// the SINGLE flow behind both the cluster's own certificate (domain == the
// persisted cluster domain) and per-host (bring-your-own) certificates. It is
// the "same table, same flow" contract: every certificate gets a
// site_certs DB row; the only divergence is at Traefik render time, where the
// main-domain row feeds the template's default-cert block and every other row
// becomes an extra tls.certificates entry referencing its own secrets.
//
// Main domain: writes the stable local copy under <configDir>/site/ (the
// source of truth `cluster update` re-materializes from), re-points the
// persisted TLS state at those files, and materializes cert_vN/key_vN.
// Per-host: no files are ever written — the cert/key live only in versioned
// Swarm secrets (hostcert-<host>_vNNN / hostkey-<host>_vNNN) + the DB row.
//
// Unless refresh is false, the full Update pipeline re-renders + re-deploys
// Traefik (content-aware: unchanged inputs deploy nothing). Requires the
// cluster to be up.
func ApplyCert(ctx context.Context, deps SiteCertDeps, configDir, version, domain, certPEM, keyPEM string, refresh bool) (*store.SiteCertRow, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("apply certificate requires a store (run `pmcluster init` + `pmcluster cluster up` first)")
	}
	if domain == "" {
		domain = deps.Store.GetSettingDefault(ctx, settingDomain, "")
		if domain == "" {
			return nil, fmt.Errorf("no persisted domain found — run `cluster up` before managing certificates")
		}
	}
	if err := tlscerts.Validate(domain); err != nil {
		return nil, err
	}
	domain = strings.ToLower(domain)

	info, err := tlscerts.ParseAndCheck(certPEM, keyPEM, domain)
	if err != nil {
		return nil, fmt.Errorf("validate certificate: %w", err)
	}

	mainCert := domain == PersistedDomain(ctx, deps.Store)
	var certName, keyName string
	if mainCert {

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

		state := tlsState{Mode: "cert", CertPath: certPath, KeyPath: keyPath}
		if err := state.save(ctx, deps.Store); err != nil {
			return nil, err
		}
	} else {

		var err error
		certName, _, err = EnsureVersionedSecret(ctx, deps.Docker, deps.Store, hostSecretBase("cert", domain), []byte(certPEM))
		if err != nil {
			return nil, fmt.Errorf("ensure host cert secret: %w", err)
		}
		keyName, _, err = EnsureVersionedSecret(ctx, deps.Docker, deps.Store, hostSecretBase("key", domain), []byte(keyPEM))
		if err != nil {
			return nil, fmt.Errorf("ensure host key secret: %w", err)
		}
	}

	if refresh {
		res, err := Update(ctx, UpdateDeps{
			Store:    deps.Store,
			Cipher:   deps.Cipher,
			Docker:   deps.Docker,
			Deployer: deps.Deployer,
			Stdout:   io.Discard,
		}, UpdateInput{ConfigDir: configDir, Version: version})
		if err != nil {
			return nil, err
		}
		if mainCert {

			certName, keyName = res.CertSecret, res.KeySecret
		}
	}

	now := time.Now().UTC()
	row := store.SiteCertRow{
		Domain:     domain,
		CertSecret: certName,
		KeySecret:  keyName,
		NotBefore:  info.NotBefore,
		NotAfter:   info.NotAfter,
		SANs:       info.SANs,
		CertHash:   store.ConfigHash(certPEM),
		KeyHash:    store.ConfigHash(keyPEM),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := deps.Store.PutSiteCert(ctx, row); err != nil {
		return nil, fmt.Errorf("store certificate metadata: %w", err)
	}
	return &row, nil
}

// RemoveCert deletes a per-host (bring-your-own) certificate: drops the
// site_certs DB row, removes its versioned Swarm secrets (best-effort) and —
// unless refresh is false — re-renders + re-deploys Traefik so the cert stops
// being served. Returns store.ErrSiteCertNotFound when the host had no stored
// cert. The cluster's OWN certificate cannot be removed (replace it instead).
func RemoveCert(ctx context.Context, deps SiteCertDeps, configDir, version, domain string, refresh bool) error {
	if deps.Store == nil {
		return fmt.Errorf("remove certificate requires a store (run `pmcluster init` + `pmcluster cluster up` first)")
	}
	domain = strings.ToLower(domain)
	if domain == PersistedDomain(ctx, deps.Store) {
		return fmt.Errorf("cannot remove the cluster's own certificate (%s) — replace it with `pmcluster tls site set`", domain)
	}
	row, err := deps.Store.GetSiteCert(ctx, domain)
	if err != nil {
		return err
	}
	if err := deps.Store.DeleteSiteCert(ctx, domain); err != nil {
		return fmt.Errorf("delete certificate metadata: %w", err)
	}

	_ = deps.Docker.SecretRemove(ctx, row.CertSecret)
	_ = deps.Docker.SecretRemove(ctx, row.KeySecret)

	if refresh {
		if _, err := Update(ctx, UpdateDeps{
			Store:    deps.Store,
			Cipher:   deps.Cipher,
			Docker:   deps.Docker,
			Deployer: deps.Deployer,
			Stdout:   io.Discard,
		}, UpdateInput{ConfigDir: configDir, Version: version}); err != nil {
			return err
		}
	}
	return nil
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
