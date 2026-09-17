// Package impl holds the local adapters that implement the service ports.
package impl

import (
	"context"
	"errors"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// TLS applies and removes the cluster's TLS certificates by materialising
// them as versioned Swarm secrets and refreshing Traefik.
type TLS struct {
	Store       *store.Store
	Cipher      *credentials.Cipher
	Docker      docker.Client
	Deployer    cluster.StackDeployer
	Provisioner *cluster.OpenObserveProvisioner
	ConfigDir   string
	Version     string
}

// NewTLS returns the local TLS adapter.
func NewTLS(
	st *store.Store,
	cipher *credentials.Cipher,
	dc docker.Client,
	deployer cluster.StackDeployer,
	provisioner *cluster.OpenObserveProvisioner,
	configDir, version string,
) service.TLSService {
	return &TLS{
		Store:       st,
		Cipher:      cipher,
		Docker:      dc,
		Deployer:    deployer,
		Provisioner: provisioner,
		ConfigDir:   configDir,
		Version:     version,
	}
}

func (t *TLS) deps() cluster.SiteCertDeps {
	return cluster.SiteCertDeps{
		Store:       t.Store,
		Cipher:      t.Cipher,
		Docker:      t.Docker,
		Deployer:    t.Deployer,
		Provisioner: t.Provisioner,
	}
}

// ready reports whether the adapter can refresh Traefik (it needs the
// encryption key to materialise the versioned Swarm secrets).
func (t *TLS) ready() error {
	if t.Cipher == nil {
		return errors.New("encryption key unavailable; cannot refresh Traefik")
	}
	return nil
}

// SiteCert applies the cluster's own (main-domain) certificate.
func (t *TLS) SiteCert(ctx context.Context, domain, certPEM, keyPEM string) (*store.SiteCertRow, error) {
	if err := t.ready(); err != nil {
		return nil, err
	}
	return cluster.ApplyCert(ctx, t.deps(), t.ConfigDir, t.Version, domain, certPEM, keyPEM, true)
}

// ApplyHostCert applies a per-host (customer-domain) certificate.
func (t *TLS) ApplyHostCert(ctx context.Context, host, certPEM, keyPEM string, refresh bool) (*store.SiteCertRow, error) {
	if err := t.ready(); err != nil {
		return nil, err
	}
	return cluster.ApplyCert(ctx, t.deps(), t.ConfigDir, t.Version, host, certPEM, keyPEM, refresh)
}

// RemoveHostCert removes a per-host certificate.
func (t *TLS) RemoveHostCert(ctx context.Context, host string, refresh bool) error {
	if err := t.ready(); err != nil {
		return err
	}
	return cluster.RemoveCert(ctx, t.deps(), t.ConfigDir, t.Version, host, refresh)
}

// GetSiteCert returns the stored metadata for one certificate.
func (t *TLS) GetSiteCert(ctx context.Context, domain string) (*store.SiteCertRow, error) {
	row, err := t.Store.GetSiteCert(ctx, domain)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// List returns all stored certificates (main + per-host rows).
func (t *TLS) List(ctx context.Context) ([]store.SiteCertRow, error) {
	rows, err := t.Store.ListSiteCerts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.SiteCertRow, 0, len(rows))
	out = append(out, rows...)
	return out, nil
}

// MainDomain is the cluster's own domain (its default certificate).
func (t *TLS) MainDomain(ctx context.Context) (string, error) {
	return cluster.PersistedDomain(ctx, t.Store), nil
}
