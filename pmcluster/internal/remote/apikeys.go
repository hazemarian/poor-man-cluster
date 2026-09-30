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
}

type apiKeyListDTO struct {
	Keys []apiKeyDTO `json:"keys"`
}

type apiKeyCreatedDTO struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Stack string `json:"stack"`
	Token string `json:"token"`
}

// Create mints an API key, optionally scoped to a single stack. The stack
// field is only sent when non-empty so older daemons (which don't know the
// field and reject unknown JSON keys) keep working for unscoped tokens.
func (a *APIKeys) Create(ctx context.Context, name string, stack ...string) (int64, string, error) {
	body := map[string]string{"name": name}
	if s := firstStack(stack); s != "" {
		body["stack"] = s
	}
	var out apiKeyCreatedDTO
	if err := a.c.do(ctx, http.MethodPost, "/api_keys", body, &out); err != nil {
		return 0, "", err
	}
	return out.ID, out.Token, nil
}

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

func (a *APIKeys) Delete(ctx context.Context, id int64) error {
	return a.c.do(ctx, http.MethodDelete, "/api_keys/"+strconv.FormatInt(id, 10), nil, nil)
}

// firstStack resolves the optional variadic stack scope: the first
// non-empty value wins, "" when no scope was supplied.
func firstStack(stack []string) string {
	for _, s := range stack {
		if s != "" {
			return s
		}
	}
	return ""
}
