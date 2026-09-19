package certs

import (
	"context"
	"errors"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Local applies and removes the cluster's TLS certificates by materialising
// them as versioned Swarm secrets and refreshing Traefik.
type Local struct {
	Store     *store.Store
	Cipher    *credentials.Cipher
	Docker    runtime.Client
	Deployer  cluster.StackDeployer
	ConfigDir string
	Version   string
}

// NewLocal returns the local certs adapter.
func NewLocal(
	st *store.Store,
	cipher *credentials.Cipher,
	dc runtime.Client,
	deployer cluster.StackDeployer,
	configDir, version string,
) *Local {
	return &Local{
		Store:     st,
		Cipher:    cipher,
		Docker:    dc,
		Deployer:  deployer,
		ConfigDir: configDir,
		Version:   version,
	}
}

// Compile-time proof that Local implements the port.
var _ Service = (*Local)(nil)

func (t *Local) deps() cluster.SiteCertDeps {
	return cluster.SiteCertDeps{
		Store:    t.Store,
		Cipher:   t.Cipher,
		Docker:   t.Docker,
		Deployer: t.Deployer,
	}
}

// ready reports whether the adapter can refresh Traefik (it needs the
// encryption key to materialise the versioned Swarm secrets).
func (t *Local) ready() error {
	if t.Cipher == nil {
		return errors.New("encryption key unavailable; cannot refresh Traefik")
	}
	return nil
}

// SiteCert applies the cluster's own (main-domain) certificate.
func (t *Local) SiteCert(ctx context.Context, domain, certPEM, keyPEM string) (*Cert, error) {
	if err := t.ready(); err != nil {
		return nil, err
	}
	row, err := cluster.ApplyCert(ctx, t.deps(), t.ConfigDir, t.Version, domain, certPEM, keyPEM, true)
	if err != nil {
		return nil, err
	}
	return rowModel(row), nil
}

// ApplyHostCert applies a per-host (customer-domain) certificate.
func (t *Local) ApplyHostCert(ctx context.Context, host, certPEM, keyPEM string, refresh bool) (*Cert, error) {
	if err := t.ready(); err != nil {
		return nil, err
	}
	row, err := cluster.ApplyCert(ctx, t.deps(), t.ConfigDir, t.Version, host, certPEM, keyPEM, refresh)
	if err != nil {
		return nil, err
	}
	return rowModel(row), nil
}

// RemoveHostCert removes a per-host certificate.
func (t *Local) RemoveHostCert(ctx context.Context, host string, refresh bool) error {
	if err := t.ready(); err != nil {
		return err
	}
	return cluster.RemoveCert(ctx, t.deps(), t.ConfigDir, t.Version, host, refresh)
}

// GetSiteCert returns the stored metadata for one certificate.
func (t *Local) GetSiteCert(ctx context.Context, domain string) (*Cert, error) {
	row, err := t.Store.GetSiteCert(ctx, domain)
	if err != nil {
		return nil, err
	}
	return rowModel(&row), nil
}

// List returns all stored certificates (main + per-host rows).
func (t *Local) List(ctx context.Context) ([]Cert, error) {
	rows, err := t.Store.ListSiteCerts(ctx)
	if err != nil {
		return nil, err
	}
	return rowModels(rows), nil
}

// MainDomain is the cluster's own domain (its default certificate).
func (t *Local) MainDomain(ctx context.Context) (string, error) {
	return cluster.PersistedDomain(ctx, t.Store), nil
}

// rowModel converts a persisted site_certs row into the domain model.
func rowModel(row *store.SiteCertRow) *Cert {
	return &Cert{
		Domain:     row.Domain,
		CertSecret: row.CertSecret,
		KeySecret:  row.KeySecret,
		NotBefore:  row.NotBefore,
		NotAfter:   row.NotAfter,
		SANs:       row.SANs,
		CertHash:   row.CertHash,
		KeyHash:    row.KeyHash,
		CreatedAt:  row.CreatedAt,
		UpdatedAt:  row.UpdatedAt,
	}
}

func rowModels(rows []store.SiteCertRow) []Cert {
	out := make([]Cert, 0, len(rows))
	for _, r := range rows {
		out = append(out, *rowModel(&r))
	}
	return out
}
