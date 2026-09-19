package remote

import (
	"context"
	"net/http"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/usage"
)

// Usage is the HTTP adapter for GET /api/usage — the config/secret → stacks
// reference graph computed by the daemon. It implements usage.Service.
type Usage struct{ c *Client }

// NewUsage builds the remote usage adapter.
func NewUsage(c *Client) *Usage { return &Usage{c: c} }

// Get returns the config/secret usage graph.
func (u *Usage) Get(ctx context.Context) (*usage.Usage, error) {
	var out usage.Usage
	if err := u.c.do(ctx, http.MethodGet, "/usage", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
