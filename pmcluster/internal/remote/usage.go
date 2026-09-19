package remote

import (
	"context"
	"net/http"
)

// Usage is the HTTP adapter for GET /api/usage — the config/secret → stacks
// reference graph computed by the daemon.
type Usage struct{ c *Client }

// NewUsage builds the remote usage adapter.
func NewUsage(c *Client) *Usage { return &Usage{c: c} }

// UsageResult is the daemon's response: which stacks reference which configs
// and secrets.
type UsageResult struct {
	Configs map[string][]string `json:"configs"`
	Secrets map[string][]string `json:"secrets"`
}

// Get returns the config/secret usage graph.
func (u *Usage) Get(ctx context.Context) (*UsageResult, error) {
	var out UsageResult
	if err := u.c.do(ctx, http.MethodGet, "/usage", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
