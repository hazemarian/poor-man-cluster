package service

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// TLSService manages the cluster's TLS certificates: the main (site)
// certificate for the cluster's own domain and per-host certificates, both
// stored as versioned Swarm secrets with a site_certs DB row.
type TLSService interface {
	// SiteCert stores the main certificate for the cluster's domain.
	SiteCert(ctx context.Context, domain, certPEM, keyPEM string) (*store.SiteCertRow, error)
	// ApplyHostCert stores a per-host certificate.
	ApplyHostCert(ctx context.Context, host, certPEM, keyPEM string) (*store.SiteCertRow, error)
	// RemoveHostCert deletes a per-host certificate.
	RemoveHostCert(ctx context.Context, host string) error
	// List returns all stored certificates (main + per-host rows).
	List(ctx context.Context) ([]store.SiteCertRow, error)
}
