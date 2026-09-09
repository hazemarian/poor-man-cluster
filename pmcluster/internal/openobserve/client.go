// Package openobserve is a minimal REST client for the OpenObserve API. It is
// used by pmcluster provisioning to create an automation admin user and a
// dedicated ingestion token on a running OpenObserve instance, so the OTel
// collector can authenticate ingestion without baking credentials into the
// OpenObserve data volume (which only reads env on first boot).
package openobserve

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to an OpenObserve instance over HTTP. BaseURL is the origin
// (scheme+host, with no trailing slash, e.g. "https://observ.example.com").
type Client struct {
	BaseURL string
	Org     string
	HTTP    *http.Client
}

// NewClient returns a Client for the given base URL and org (the org is the
// API path segment, typically "default").
func NewClient(baseURL, org string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Org:     org,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// User is the subset of the OO users API payload pmcluster needs to create an
// admin user.
type User struct {
	Email     string
	FirstName string
	LastName  string
	Password  string
	Role      string
}

// createUserPayload is the wire shape of POST /api/{org}/users.
type createUserPayload struct {
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Password  string `json:"password"`
	Role      string `json:"role"`
}

// ingestionToken is the subset of the OO ingestion-tokens list/create payload
// pmcluster needs to identify and read a token.
type ingestionToken struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// basicAuth returns an "Authorization: Basic ..." header value for the given
// username:password (root admin, or org:token for ingestion).
func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// do performs a JSON request authenticated with the given username:password,
// decoding a successful (2xx) response into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, authUser, authPass string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(raw)
	}

	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", basicAuth(authUser, authPass))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("openobserve %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}

// listUsers returns the current users of the org.
func (c *Client) listUsers(ctx context.Context, authUser, authPass string) ([]ingestionUser, error) {
	var resp struct {
		Data []ingestionUser `json:"data"`
	}
	if err := c.do(ctx, "GET", "/api/"+c.Org+"/users", authUser, authPass, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

type ingestionUser struct {
	Email string `json:"email"`
}

// EnsureUser creates the given admin user unless one with the same email
// already exists. Idempotent: re-running with the same email is a no-op.
// Authenticates with a root/mgmt user (email,password).
func (c *Client) EnsureUser(ctx context.Context, rootUser, rootPass string, u User) error {
	existing, err := c.listUsers(ctx, rootUser, rootPass)
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	for _, e := range existing {
		if strings.EqualFold(e.Email, u.Email) {
			return nil
		}
	}
	return c.do(ctx, "POST", "/api/"+c.Org+"/users", rootUser, rootPass, createUserPayload(u), nil)
}

// listIngestionTokens returns the org's ingestion tokens (for idempotent lookup
// by name; note OpenObserve has no DELETE for tokens).
func (c *Client) listIngestionTokens(ctx context.Context, authUser, authPass string) ([]ingestionToken, error) {
	var resp struct {
		Data []ingestionToken `json:"data"`
	}
	if err := c.do(ctx, "GET", "/api/"+c.Org+"/ingestion-tokens", authUser, authPass, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// EnsureIngestionToken returns a dedicated ingestion token with the given name,
// creating one if it does not yet exist. Idempotent: an existing token with
// that name is reused. Authenticates with a root/mgmt user (email,password).
func (c *Client) EnsureIngestionToken(ctx context.Context, rootUser, rootPass, name string) (string, error) {
	existing, err := c.listIngestionTokens(ctx, rootUser, rootPass)
	if err != nil {
		return "", fmt.Errorf("list ingestion tokens: %w", err)
	}
	for _, t := range existing {
		if t.Name == name && t.Token != "" {
			return t.Token, nil
		}
	}

	var resp struct {
		Data ingestionToken `json:"data"`
	}
	payload := struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}{Name: name, Description: "pmcluster-managed ingestion token"}
	if err := c.do(ctx, "POST", "/api/"+c.Org+"/ingestion-tokens", rootUser, rootPass, payload, &resp); err != nil {
		return "", err
	}
	if resp.Data.Token == "" {
		return "", fmt.Errorf("openobserve returned an empty ingestion token")
	}
	return resp.Data.Token, nil
}
