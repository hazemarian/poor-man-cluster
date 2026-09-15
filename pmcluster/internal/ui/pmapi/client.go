// Package pmapi is a typed client for the pmcluster HTTP API.
//
// It is the "model" half of the UI's MVC split: controllers hold no curl-like
// JSON munging, they call these methods and get typed structs. The client talks
// to the daemon's /api/* routes with a Bearer token that can be swapped at
// runtime (the Settings page updates it without a restart).
package pmapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Error is a non-2xx response from the daemon, carrying the upstream status and
// message so the UI can surface real errors to the operator.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("pmcluster API %d: %s", e.Status, e.Body)
}

// Client calls the pmcluster daemon API. Base is e.g. "http://host.docker.internal:9090"
// or "https://pmcluster.nextrum-sy.com"; paths are joined under /api.
type Client struct {
	http *http.Client
	mu   sync.RWMutex
	base string
	tok  string
}

// New builds a Client. base and token are initial values; both are mutable via
// SetBase/SetToken so the UI can reconfigure after boot.
func New(base, token string, timeout time.Duration) *Client {
	return &Client{
		http: &http.Client{Timeout: timeout},
		base: strings.TrimRight(base, "/"),
		tok:  token,
	}
}

// SetToken swaps the Bearer token used on subsequent calls.
func (c *Client) SetToken(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tok = tok
}

// SetBase swaps the daemon base URL used on subsequent calls.
func (c *Client) SetBase(base string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.base = strings.TrimRight(base, "/")
}

// Configured reports whether a base URL + token are currently set.
func (c *Client) Configured() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.base != "" && c.tok != ""
}

func (c *Client) baseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.base
}

func (c *Client) token() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tok
}

// do performs a JSON request against /api{path}, decoding a 2xx body into out.
// Non-2xx responses (and transport errors) return a *Error.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}

	urlStr := c.baseURL() + "/api" + path
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token())
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// ---- typed endpoint methods ----

// Me returns the authenticated daemon user.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var m Me
	err := c.do(ctx, http.MethodGet, "/me", nil, &m)
	return &m, err
}

// ClusterInfo returns the daemon node + swarm summary.
func (c *Client) ClusterInfo(ctx context.Context) (*ClusterInfo, error) {
	var ci ClusterInfo
	err := c.do(ctx, http.MethodGet, "/cluster/info", nil, &ci)
	return &ci, err
}

// Nodes returns the swarm node list.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var body struct {
		Nodes []Node `json:"nodes"`
	}
	err := c.do(ctx, http.MethodGet, "/nodes", nil, &body)
	return body.Nodes, err
}

// ListStacks returns all known stacks.
func (c *Client) ListStacks(ctx context.Context) ([]Stack, error) {
	var body struct {
		Stacks []Stack `json:"stacks"`
	}
	err := c.do(ctx, http.MethodGet, "/stacks", nil, &body)
	return body.Stacks, err
}

// GetStack returns one stack's metadata + recent revisions.
func (c *Client) GetStack(ctx context.Context, name string) (*StackDetail, error) {
	var d StackDetail
	err := c.do(ctx, http.MethodGet, "/stacks/"+url.PathEscape(name), nil, &d)
	return &d, err
}

// GetRevision returns one revision's full source + rendered YAML.
func (c *Client) GetRevision(ctx context.Context, name string, rev int64) (*Revision, error) {
	var r Revision
	p := "/stacks/" + url.PathEscape(name) + "/revisions/" + strconv.FormatInt(rev, 10)
	err := c.do(ctx, http.MethodGet, p, nil, &r)
	return &r, err
}

// Deploy submits a manifest to POST /api/stacks.
func (c *Client) Deploy(ctx context.Context, p DeployPayload) (*DeployResult, error) {
	var r DeployResult
	err := c.do(ctx, http.MethodPost, "/stacks", p, &r)
	return &r, err
}

// Rollback reverts a stack to a prior revision.
func (c *Client) Rollback(ctx context.Context, name string, rev int64) (*RollbackResult, error) {
	var r RollbackResult
	body := map[string]any{"revision": rev}
	err := c.do(ctx, http.MethodPost, "/stacks/"+url.PathEscape(name)+"/rollback", body, &r)
	return &r, err
}

// ListBackups returns the most recent backups across stacks.
func (c *Client) ListBackups(ctx context.Context, limit int) ([]Backup, error) {
	var body struct {
		Backups []Backup `json:"backups"`
	}
	err := c.do(ctx, http.MethodGet, "/backups?limit="+strconv.Itoa(limit), nil, &body)
	return body.Backups, err
}

// CreateBackup triggers a cluster-wide backup.
func (c *Client) CreateBackup(ctx context.Context) (*Backup, error) {
	var b Backup
	err := c.do(ctx, http.MethodPost, "/backups", nil, &b)
	return &b, err
}

// ListStackBackups returns backups for a specific stack.
func (c *Client) ListStackBackups(ctx context.Context, name string) ([]Backup, error) {
	var body struct {
		Backups []Backup `json:"backups"`
	}
	err := c.do(ctx, http.MethodGet, "/stacks/"+url.PathEscape(name)+"/backups", nil, &body)
	return body.Backups, err
}

// ListHostCerts returns every per-host TLS certificate.
func (c *Client) ListHostCerts(ctx context.Context) ([]HostCert, error) {
	var out hostCertsResponse
	err := c.do(ctx, http.MethodGet, "/tls/hosts", nil, &out)
	return out.Hosts, err
}

// AddHostCert stores a per-host cert/key (text) and triggers the Traefik
// refresh on the daemon.
func (c *Client) AddHostCert(ctx context.Context, host, cert, key string) (*HostCert, error) {
	body := map[string]string{"cert": cert, "key": key}
	var out HostCert
	err := c.do(ctx, http.MethodPut, "/tls/hosts/"+url.PathEscape(host), body, &out)
	return &out, err
}

// RemoveHostCert deletes a per-host cert and triggers the Traefik refresh.
func (c *Client) RemoveHostCert(ctx context.Context, host string) error {
	return c.do(ctx, http.MethodDelete, "/tls/hosts/"+url.PathEscape(host), nil, nil)
}

// ListWebhooks returns every deploy-webhook source (without secrets).
func (c *Client) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	var body struct {
		Webhooks []Webhook `json:"webhooks"`
	}
	err := c.do(ctx, http.MethodGet, "/webhooks", nil, &body)
	return body.Webhooks, err
}

// CreateWebhook creates a webhook source and returns its one-time shared secret.
func (c *Client) CreateWebhook(ctx context.Context, source, description string) (*WebhookCreated, error) {
	body := map[string]string{"source": source, "description": description}
	var out WebhookCreated
	err := c.do(ctx, http.MethodPost, "/webhooks", body, &out)
	return &out, err
}

// DeleteWebhook removes a webhook source, revoking its shared secret.
func (c *Client) DeleteWebhook(ctx context.Context, source string) error {
	return c.do(ctx, http.MethodDelete, "/webhooks/"+url.PathEscape(source), nil, nil)
}

// ListAPIKeys returns every API user (without token material).
func (c *Client) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	var body struct {
		Keys []APIKey `json:"keys"`
	}
	err := c.do(ctx, http.MethodGet, "/api_keys", nil, &body)
	return body.Keys, err
}

// CreateAPIKey creates an API user and returns its one-time bearer token.
func (c *Client) CreateAPIKey(ctx context.Context, name string) (*APIKeyCreated, error) {
	body := map[string]string{"name": name}
	var out APIKeyCreated
	err := c.do(ctx, http.MethodPost, "/api_keys", body, &out)
	return &out, err
}

// DeleteAPIKey removes an API user by id, revoking its bearer token.
func (c *Client) DeleteAPIKey(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/api_keys/%d", id), nil, nil)
}
