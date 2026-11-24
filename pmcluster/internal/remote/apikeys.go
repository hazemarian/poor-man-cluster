package remote

import (
	"context"
	"net/http"
	"strconv"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/apikeys"
)

// APIKeys is the HTTP adapter for the apikeys domain Service port.
type APIKeys struct{ c *Client }

// NewAPIKeys builds the remote api-keys adapter.
func NewAPIKeys(c *Client) apikeys.Service { return &APIKeys{c: c} }

type apiKeyDTO struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
	Stack      string `json:"stack"`
	Role       string `json:"role"`
	Kind       string `json:"kind"`
}

type apiKeyListDTO struct {
	Keys []apiKeyDTO `json:"keys"`
}

type apiKeyCreatedDTO struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Stack string `json:"stack"`
	Role  string `json:"role"`
	Kind  string `json:"kind"`
	Token string `json:"token"`
}

// Create mints an API key with a stack scope, a role and a surface.
//
// Every optional field is sent only when it differs from the daemon's default,
// so a call that asks for the historical behaviour posts exactly the body it
// always did — and an older daemon (which rejects unknown JSON keys) keeps
// working for the common case.
func (a *APIKeys) Create(ctx context.Context, name, stack, role, kind string) (int64, string, error) {
	body := map[string]string{"name": name}
	if stack != "" {
		body["stack"] = stack
	}
	if role != "" {
		body["role"] = role
	}
	if kind != "" {
		body["kind"] = kind
	}
	var out apiKeyCreatedDTO
	if err := a.c.do(ctx, http.MethodPost, "/api_keys", body, &out); err != nil {
		return 0, "", err
	}
	return out.ID, out.Token, nil
}

// List returns the minted API keys via GET /api_keys.
func (a *APIKeys) List(ctx context.Context) ([]apikeys.APIKey, error) {
	var out apiKeyListDTO
	if err := a.c.do(ctx, http.MethodGet, "/api_keys", nil, &out); err != nil {
		return nil, err
	}
	keys := make([]apikeys.APIKey, 0, len(out.Keys))
	for _, d := range out.Keys {
		keys = append(keys, apikeys.APIKey{ID: d.ID, Name: d.Name, CreatedAt: d.CreatedAt, LastUsedAt: d.LastUsedAt, Stack: d.Stack})
	}
	return keys, nil
}

// Delete revokes an API key by row ID via DELETE /api_keys/{id}.
func (a *APIKeys) Delete(ctx context.Context, id int64) error {
	return a.c.do(ctx, http.MethodDelete, "/api_keys/"+strconv.FormatInt(id, 10), nil, nil)
}
