package remote

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/certs"
)

// TLS is the HTTP adapter for certs.Service.
type TLS struct{ c *Client }

// NewTLS builds the remote certs adapter.
func NewTLS(c *Client) certs.Service { return &TLS{c: c} }

// Compile-time proof that TLS implements the port.
var _ certs.Service = (*TLS)(nil)

type certDTO struct {
	Domain     string   `json:"domain,omitempty"`
	Host       string   `json:"host,omitempty"`
	CertSecret string   `json:"cert_secret"`
	KeySecret  string   `json:"key_secret"`
	NotBefore  string   `json:"not_before"`
	NotAfter   string   `json:"not_after"`
	SANs       []string `json:"sans"`
	CertHash   string   `json:"cert_hash"`
	KeyHash    string   `json:"key_hash"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
}

type certListDTO struct {
	Hosts []certDTO `json:"hosts"`
}

func (a *TLS) SiteCert(ctx context.Context, domain, certPEM, keyPEM string) (*certs.Cert, error) {
	var out certDTO
	if err := a.c.do(ctx, http.MethodPut, "/tls/site", map[string]string{
		"cert": certPEM,
		"key":  keyPEM,
	}, &out); err != nil {
		return nil, err
	}
	return out.model(), nil
}

func (a *TLS) ApplyHostCert(ctx context.Context, host, certPEM, keyPEM string, refresh bool) (*certs.Cert, error) {
	var out certDTO
	if err := a.c.do(ctx, http.MethodPut, "/tls/hosts/"+url.PathEscape(host), map[string]string{
		"cert": certPEM,
		"key":  keyPEM,
	}, &out); err != nil {
		return nil, err
	}
	return out.model(), nil
}

func (a *TLS) RemoveHostCert(ctx context.Context, host string, refresh bool) error {
	return a.c.do(ctx, http.MethodDelete, "/tls/hosts/"+url.PathEscape(host), nil, nil)
}

// GetSiteCert returns the cluster's main certificate. The API serves the
// persisted cluster domain, so the domain argument is informational.
func (a *TLS) GetSiteCert(ctx context.Context, domain string) (*certs.Cert, error) {
	var out certDTO
	if err := a.c.do(ctx, http.MethodGet, "/tls/site", nil, &out); err != nil {
		return nil, err
	}
	return out.model(), nil
}

// List returns the per-host certificates (the API already excludes the
// cluster's own domain server-side).
func (a *TLS) List(ctx context.Context) ([]certs.Cert, error) {
	var out certListDTO
	if err := a.c.do(ctx, http.MethodGet, "/tls/hosts", nil, &out); err != nil {
		return nil, err
	}
	rows := make([]certs.Cert, 0, len(out.Hosts))
	for _, d := range out.Hosts {
		rows = append(rows, *d.model())
	}
	return rows, nil
}

// MainDomain resolves the cluster's own domain from the main certificate.
func (a *TLS) MainDomain(ctx context.Context) (string, error) {
	row, err := a.GetSiteCert(ctx, "")
	if err != nil {
		return "", err
	}
	return row.Domain, nil
}

func (d certDTO) model() *certs.Cert {
	return &certs.Cert{
		Domain:     d.Domain,
		CertSecret: d.CertSecret,
		KeySecret:  d.KeySecret,
		NotBefore:  parseRFC3339(d.NotBefore),
		NotAfter:   parseRFC3339(d.NotAfter),
		SANs:       d.SANs,
		CertHash:   d.CertHash,
		KeyHash:    d.KeyHash,
		CreatedAt:  parseRFC3339(d.CreatedAt),
		UpdatedAt:  parseRFC3339(d.UpdatedAt),
	}
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
