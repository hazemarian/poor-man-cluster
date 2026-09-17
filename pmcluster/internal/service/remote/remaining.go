package remote

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Webhooks is the HTTP adapter for service.WebhooksService.
type Webhooks struct{ c *Client }

// NewWebhooks builds the remote webhooks adapter.
func NewWebhooks(c *Client) service.WebhooksService { return &Webhooks{c: c} }

type webhookDTO struct {
	Source      string `json:"source"`
	Description string `json:"description,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	LastUsedAt  int64  `json:"last_used_at,omitempty"`
}

type webhookListDTO struct {
	Webhooks []webhookDTO `json:"webhooks"`
}

type webhookCreatedDTO struct {
	Source string `json:"source"`
	Secret string `json:"secret"`
}

func (a *Webhooks) Create(ctx context.Context, source, description string) (string, error) {
	var out webhookCreatedDTO
	err := a.c.do(ctx, http.MethodPost, "/webhooks", map[string]string{
		"source":      source,
		"description": description,
	}, &out)
	return out.Secret, err
}

func (a *Webhooks) List(ctx context.Context) ([]*store.WebhookSource, error) {
	var out webhookListDTO
	if err := a.c.do(ctx, http.MethodGet, "/webhooks", nil, &out); err != nil {
		return nil, err
	}
	sources := make([]*store.WebhookSource, 0, len(out.Webhooks))
	for _, d := range out.Webhooks {
		var desc sql.NullString
		if d.Description != "" {
			desc = sql.NullString{String: d.Description, Valid: true}
		}
		var used sql.NullInt64
		if d.LastUsedAt != 0 {
			used = sql.NullInt64{Int64: d.LastUsedAt, Valid: true}
		}
		sources = append(sources, &store.WebhookSource{
			Source:      d.Source,
			Description: desc,
			CreatedAt:   d.CreatedAt,
			LastUsedAt:  used,
		})
	}
	return sources, nil
}

func (a *Webhooks) Delete(ctx context.Context, source string) error {
	return a.c.do(ctx, http.MethodDelete, "/webhooks/"+url.PathEscape(source), nil, nil)
}

// APIKeys is the HTTP adapter for service.APIKeysService.
type APIKeys struct{ c *Client }

// NewAPIKeys builds the remote api-keys adapter.
func NewAPIKeys(c *Client) service.APIKeysService { return &APIKeys{c: c} }

type apiKeyDTO struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
}

type apiKeyListDTO struct {
	Keys []apiKeyDTO `json:"keys"`
}

type apiKeyCreatedDTO struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

func (a *APIKeys) Create(ctx context.Context, name string) (int64, string, error) {
	var out apiKeyCreatedDTO
	if err := a.c.do(ctx, http.MethodPost, "/api_keys", map[string]string{"name": name}, &out); err != nil {
		return 0, "", err
	}
	return out.ID, out.Token, nil
}

func (a *APIKeys) List(ctx context.Context) ([]store.UserRow, error) {
	var out apiKeyListDTO
	if err := a.c.do(ctx, http.MethodGet, "/api_keys", nil, &out); err != nil {
		return nil, err
	}
	keys := make([]store.UserRow, 0, len(out.Keys))
	for _, d := range out.Keys {
		keys = append(keys, store.UserRow{ID: d.ID, Name: d.Name, CreatedAt: d.CreatedAt})
	}
	return keys, nil
}

func (a *APIKeys) Delete(ctx context.Context, id int64) error {
	return a.c.do(ctx, http.MethodDelete, "/api_keys/"+strconv.FormatInt(id, 10), nil, nil)
}

// Backups is the HTTP adapter for service.BackupsService.
type Backups struct{ c *Client }

// NewBackups builds the remote backups adapter.
func NewBackups(c *Client) service.BackupsService { return &Backups{c: c} }

type backupDTO struct {
	ID           int64    `json:"id"`
	Status       string   `json:"status"`
	StackName    string   `json:"stack_name,omitempty"`
	Revision     int64    `json:"revision,omitempty"`
	ArchivePaths []string `json:"archive_paths"`
	ErrorMessage string   `json:"error_message,omitempty"`
	StartedAt    int64    `json:"started_at"`
	FinishedAt   int64    `json:"finished_at,omitempty"`
}

type backupListDTO struct {
	Backups []backupDTO `json:"backups"`
}

type backupCreatedDTO struct {
	ID           int64    `json:"id"`
	Status       string   `json:"status"`
	ArchivePaths []string `json:"archive_paths"`
}

func (a *Backups) Trigger(ctx context.Context, stackName string, revision int64) (int64, []string, error) {
	var out backupCreatedDTO
	if err := a.c.do(ctx, http.MethodPost, "/backups", nil, &out); err != nil {
		return 0, nil, err
	}
	return out.ID, out.ArchivePaths, nil
}

func (a *Backups) List(ctx context.Context, limit int) ([]*store.Backup, error) {
	var out backupListDTO
	path := "/backups"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	if err := a.c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return a.toRows(out.Backups), nil
}

func (a *Backups) ListForStack(ctx context.Context, stackName string) ([]*store.Backup, error) {
	var out backupListDTO
	if err := a.c.do(ctx, http.MethodGet, "/stacks/"+url.PathEscape(stackName)+"/backups", nil, &out); err != nil {
		return nil, err
	}
	return a.toRows(out.Backups), nil
}

func (a *Backups) toRows(dtos []backupDTO) []*store.Backup {
	rows := make([]*store.Backup, 0, len(dtos))
	for _, d := range dtos {
		var stack sql.NullString
		if d.StackName != "" {
			stack = sql.NullString{String: d.StackName, Valid: true}
		}
		var rev sql.NullInt64
		if d.Revision != 0 {
			rev = sql.NullInt64{Int64: d.Revision, Valid: true}
		}
		var finished sql.NullInt64
		if d.FinishedAt != 0 {
			finished = sql.NullInt64{Int64: d.FinishedAt, Valid: true}
		}
		rows = append(rows, &store.Backup{
			ID:           d.ID,
			StackName:    stack,
			Revision:     rev,
			Status:       d.Status,
			ArchivePaths: joinStrings(d.ArchivePaths),
			ErrorMessage: d.ErrorMessage,
			StartedAt:    d.StartedAt,
			FinishedAt:   finished,
		})
	}
	return rows
}

// TLS is the HTTP adapter for service.TLSService.
type TLS struct{ c *Client }

// NewTLS builds the remote TLS adapter.
func NewTLS(c *Client) service.TLSService { return &TLS{c: c} }

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

func (a *TLS) SiteCert(ctx context.Context, domain, certPEM, keyPEM string) (*store.SiteCertRow, error) {
	var out certDTO
	if err := a.c.do(ctx, http.MethodPut, "/tls/site", map[string]string{
		"cert": certPEM,
		"key":  keyPEM,
	}, &out); err != nil {
		return nil, err
	}
	return out.row(), nil
}

func (a *TLS) ApplyHostCert(ctx context.Context, host, certPEM, keyPEM string, refresh bool) (*store.SiteCertRow, error) {
	var out certDTO
	if err := a.c.do(ctx, http.MethodPut, "/tls/hosts/"+url.PathEscape(host), map[string]string{
		"cert": certPEM,
		"key":  keyPEM,
	}, &out); err != nil {
		return nil, err
	}
	return out.row(), nil
}

func (a *TLS) RemoveHostCert(ctx context.Context, host string, refresh bool) error {
	return a.c.do(ctx, http.MethodDelete, "/tls/hosts/"+url.PathEscape(host), nil, nil)
}

// GetSiteCert returns the cluster's main certificate. The API serves the
// persisted cluster domain, so the domain argument is informational.
func (a *TLS) GetSiteCert(ctx context.Context, domain string) (*store.SiteCertRow, error) {
	var out certDTO
	if err := a.c.do(ctx, http.MethodGet, "/tls/site", nil, &out); err != nil {
		return nil, err
	}
	return out.row(), nil
}

// List returns the per-host certificates (the API already excludes the
// cluster's own domain server-side).
func (a *TLS) List(ctx context.Context) ([]store.SiteCertRow, error) {
	var out certListDTO
	if err := a.c.do(ctx, http.MethodGet, "/tls/hosts", nil, &out); err != nil {
		return nil, err
	}
	rows := make([]store.SiteCertRow, 0, len(out.Hosts))
	for _, d := range out.Hosts {
		rows = append(rows, *d.row())
	}
	return rows, nil
}

// MainDomain resolves the cluster's own domain from the main certificate.
func (a *TLS) MainDomain(ctx context.Context) (string, error) {
	row, err := a.GetSiteCert(ctx, "")
	if err != nil {
		return "", nil
	}
	return row.Domain, nil
}

func (d certDTO) row() *store.SiteCertRow {
	return &store.SiteCertRow{
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

func joinStrings(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
