// Package remote implements the service ports against the pmcluster daemon
// REST API. It is the second adapter behind the same interfaces that the
// local impl package provides, so a CLI can run off-node over HTTP while the
// command bodies stay unchanged.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Client calls the pmcluster daemon API. Base is e.g.
// "https://pmcluster.nextrum-sy.com" or "http://127.0.0.1:9090"; paths are
// joined under /api and authenticated with a Bearer token.
type Client struct {
	http *http.Client
	base string
	tok  string
}

// New builds a Client for the given base URL and bearer token.
func New(base, token string, timeout time.Duration) *Client {
	return &Client{
		http: &http.Client{Timeout: timeout},
		base: strings.TrimRight(base, "/"),
		tok:  token,
	}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api"+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("pmcluster API: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapError(resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// mapError converts a non-2xx daemon response into the closest store or
// service sentinel so errors.Is-based mapping keeps working over HTTP.
func mapError(status int, body string) error {
	msg := body
	if msg == "" {
		msg = http.StatusText(status)
	}
	base := fmt.Errorf("pmcluster API %d: %s", status, msg)
	has := func(s string) bool { return strings.Contains(body, s) }
	switch {
	case status == http.StatusNotFound && has("stack"):
		return store.ErrStackNotFound
	case status == http.StatusNotFound && has("revision"):
		return store.ErrRevisionNotFound
	case status == http.StatusNotFound && has("config"):
		return store.ErrConfigNotFound
	case status == http.StatusNotFound && has("secret"):
		return store.ErrSecretNotFound
	case status == http.StatusNotFound && has("webhook"):
		return store.ErrWebhookSourceNotFound
	case status == http.StatusNotFound && (has("user") || has("api key")):
		return store.ErrUserNotFound
	case status == http.StatusNotFound && has("cert"):
		return store.ErrSiteCertNotFound
	case status == http.StatusConflict && has("webhook"):
		return store.ErrWebhookSourceExists
	case status == http.StatusConflict && has("config"):
		return store.ErrConfigExists
	case status == http.StatusConflict && has("secret"):
		return store.ErrSecretExists
	case status == http.StatusConflict && has("edge"):
		return service.ErrEdgeUserProtected
	case status == http.StatusConflict && has("authenticated"):
		return service.ErrSelfDelete
	case status == http.StatusServiceUnavailable:
		return service.ErrBackupTriggerNotConfigured
	}
	return base
}
