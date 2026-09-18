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

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
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

// SyncStack re-runs the deploy pipeline from the stack's latest stored
// manifest (k8s-style reconcile after config edits).
func (c *Client) SyncStack(ctx context.Context, name string) (*DeployResult, error) {
	var r DeployResult
	err := c.do(ctx, http.MethodPost, "/stacks/"+url.PathEscape(name)+"/sync", nil, &r)
	return &r, err
}

// DeleteStack undeploys the stack from the Swarm and removes its record.
func (c *Client) DeleteStack(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/stacks/"+url.PathEscape(name), nil, nil)
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

// GetSiteCert returns the cluster's own (default) certificate metadata.
func (c *Client) GetSiteCert(ctx context.Context) (*SiteCert, error) {
	var out SiteCert
	err := c.do(ctx, http.MethodGet, "/tls/site", nil, &out)
	return &out, err
}

// UpdateSiteCert uploads a new default certificate + key (PEM text). The daemon
// validates the pair against the cluster domain, materializes it as versioned
// Swarm secrets, refreshes Traefik and records the metadata.
func (c *Client) UpdateSiteCert(ctx context.Context, cert, key string) (*SiteCert, error) {
	body := map[string]string{"cert": cert, "key": key}
	var out SiteCert
	err := c.do(ctx, http.MethodPut, "/tls/site", body, &out)
	return &out, err
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

// ListSecrets returns the stored secrets matching scope and stack (empty
// values match everything). Payload is never returned.
func (c *Client) ListSecrets(ctx context.Context, scope, stack string) ([]Secret, error) {
	var body struct {
		Secrets []Secret `json:"secrets"`
	}
	err := c.do(ctx, http.MethodGet, "/secrets?scope="+url.QueryEscape(scope)+"&stack="+url.QueryEscape(stack), nil, &body)
	return body.Secrets, err
}

// CreateSecret stores a secret payload (encrypted at rest) and returns its row.
func (c *Client) CreateSecret(ctx context.Context, scope, stack, name, value string) (*Secret, error) {
	body := map[string]string{"scope": scope, "stack": stack, "name": name, "value": value}
	var out Secret
	err := c.do(ctx, http.MethodPost, "/secrets", body, &out)
	return &out, err
}

// UpdateSecret replaces a stored secret's payload (the value is encrypted at
// rest; the row's scope/stack/name are preserved).
func (c *Client) UpdateSecret(ctx context.Context, name, value string) (*Secret, error) {
	body := map[string]string{"value": value}
	var out Secret
	err := c.do(ctx, http.MethodPut, "/secrets/"+url.PathEscape(name), body, &out)
	return &out, err
}

// DeleteSecret removes a stored secret by name.
func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/secrets/"+url.PathEscape(name), nil, nil)
}

// RevealSecret returns the decrypted plaintext of a stored secret. The daemon
// decrypts it on demand; the value is never cached client-side.
func (c *Client) RevealSecret(ctx context.Context, name string) (*SecretValue, error) {
	var out SecretValue
	err := c.do(ctx, http.MethodGet, "/secrets/"+url.PathEscape(name)+"/value", nil, &out)
	return &out, err
}

// ListConfigs returns the stored configs matching scope and stack (empty
// values match everything), without content.
func (c *Client) ListConfigs(ctx context.Context, scope, stack string) ([]Config, error) {
	var body struct {
		Configs []Config `json:"configs"`
	}
	err := c.do(ctx, http.MethodGet, "/configs?scope="+url.QueryEscape(scope)+"&stack="+url.QueryEscape(stack), nil, &body)
	return body.Configs, err
}

// GetConfig returns one config including its content.
func (c *Client) GetConfig(ctx context.Context, name string) (*Config, error) {
	var out Config
	err := c.do(ctx, http.MethodGet, "/configs/"+url.PathEscape(name), nil, &out)
	return &out, err
}

// CreateConfig stores a config value.
func (c *Client) CreateConfig(ctx context.Context, scope, stack, name, kind, content string) (*Config, error) {
	body := map[string]string{"scope": scope, "stack": stack, "name": name, "kind": kind, "content": content}
	var out Config
	err := c.do(ctx, http.MethodPost, "/configs", body, &out)
	return &out, err
}

// UpdateConfig replaces a config's content (old content is pushed to history).
func (c *Client) UpdateConfig(ctx context.Context, name, content string) (*Config, error) {
	body := map[string]string{"content": content}
	var out Config
	err := c.do(ctx, http.MethodPut, "/configs/"+url.PathEscape(name), body, &out)
	return &out, err
}

// DeleteConfig removes a config and its version history.
func (c *Client) DeleteConfig(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/configs/"+url.PathEscape(name), nil, nil)
}

// ConfigVersions returns a config's version history (newest first).
func (c *Client) ConfigVersions(ctx context.Context, name string) ([]ConfigVersion, error) {
	var body struct {
		Versions []ConfigVersion `json:"versions"`
	}
	err := c.do(ctx, http.MethodGet, "/configs/"+url.PathEscape(name)+"/versions", nil, &body)
	return body.Versions, err
}

// RollbackConfig restores a config to a prior version.
func (c *Client) RollbackConfig(ctx context.Context, name string, versionID int64) (*Config, error) {
	body := map[string]any{"version_id": versionID}
	var out Config
	err := c.do(ctx, http.MethodPost, "/configs/"+url.PathEscape(name)+"/rollback", body, &out)
	return &out, err
}

// TriggerUpdate re-runs `cluster update` on the daemon (applies edited cluster
// configs/secrets to the Swarm side) and returns the result summary.
func (c *Client) TriggerUpdate(ctx context.Context) (*UpdateSummary, error) {
	var out UpdateSummary
	err := c.do(ctx, http.MethodPost, "/update", nil, &out)
	return &out, err
}

// ListRenderedConfigs returns the platform configs (infra/observability/backup/
// edge stacks, OTel collector config, Traefik dynamic config) after template
// substitution — the exact YAML the daemon would send to the Swarm. Read-only.
func (c *Client) ListRenderedConfigs(ctx context.Context) ([]RenderedConfig, error) {
	var out renderedConfigsResponse
	if err := c.do(ctx, http.MethodGet, "/cluster/rendered", nil, &out); err != nil {
		return nil, err
	}
	return out.Configs, nil
}

// ListServices returns every swarm service (all stacks), or one stack's
// services when stack is non-empty.
func (c *Client) ListServices(ctx context.Context, stack string) ([]Service, error) {
	var body struct {
		Services []Service `json:"services"`
	}
	path := "/services"
	if stack != "" {
		path += "/" + url.PathEscape(stack)
	}
	err := c.do(ctx, http.MethodGet, path, nil, &body)
	return body.Services, err
}

// ServiceTasks returns a service's task (crash/restart) history.
func (c *Client) ServiceTasks(ctx context.Context, stack, service string) ([]ServiceTask, error) {
	var body struct {
		Tasks []ServiceTask `json:"tasks"`
	}
	err := c.do(ctx, http.MethodGet, svcPath(stack, service)+"/tasks", nil, &body)
	return body.Tasks, err
}

// ServiceLogs tails a service's stdout/stderr.
func (c *Client) ServiceLogs(ctx context.Context, stack, service string, tail int) ([]ServiceLogLine, error) {
	var body struct {
		Logs []ServiceLogLine `json:"logs"`
	}
	err := c.do(ctx, http.MethodGet, svcPath(stack, service)+"/logs?tail="+strconv.Itoa(tail), nil, &body)
	return body.Logs, err
}

// RestartService forces a rolling restart of a service.
func (c *Client) RestartService(ctx context.Context, stack, service string) error {
	return c.do(ctx, http.MethodPost, svcPath(stack, service)+"/restart", nil, nil)
}

// ExecService runs a non-interactive command in a service's running task.
func (c *Client) ExecService(ctx context.Context, stack, service string, argv []string) (*ExecResult, error) {
	var out ExecResult
	err := c.do(ctx, http.MethodPost, svcPath(stack, service)+"/exec", map[string]any{"argv": argv}, &out)
	return &out, err
}

func svcPath(stack, service string) string {
	return "/services/" + url.PathEscape(stack) + "/" + url.PathEscape(service)
}
