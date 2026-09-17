// Package certs is the bounded context for TLS certificates: the cluster's
// own (main-domain) certificate and per-host (bring-your-own) certificates.
// Both flows are identical — the only difference is the target host. PEM
// bytes live in versioned Swarm secrets; the Cert model records only where
// they are, what they cover and when they expire (for the console's expiry
// warnings). The name is "certs" rather than "tls" to avoid aliasing
// crypto/tls everywhere.
package certs

import (
	"context"
	"time"
)

// Cert is the stored metadata for one TLS certificate — either the cluster's
// own (main-domain) certificate or a per-host certificate.
type Cert struct {
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

// Service manages the cluster's TLS certificates.
type Service interface {
	// SiteCert stores the main certificate for the cluster's domain.
	SiteCert(ctx context.Context, domain, certPEM, keyPEM string) (*Cert, error)
	// ApplyHostCert stores a per-host certificate.
	ApplyHostCert(ctx context.Context, host, certPEM, keyPEM string, refresh bool) (*Cert, error)
	// RemoveHostCert deletes a per-host certificate.
	RemoveHostCert(ctx context.Context, host string, refresh bool) error
	// GetSiteCert returns the stored metadata for one certificate.
	GetSiteCert(ctx context.Context, domain string) (*Cert, error)
	// List returns all stored certificates (main + per-host rows).
	List(ctx context.Context) ([]Cert, error)
	// MainDomain is the cluster's own domain (its default certificate).
	MainDomain(ctx context.Context) (string, error)
}
