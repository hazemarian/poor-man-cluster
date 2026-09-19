package remote

import (
	"context"
	"net/http"
	"strconv"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/apikeys"
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

func (a *APIKeys) List(ctx context.Context) ([]apikeys.APIKey, error) {
	var out apiKeyListDTO
	if err := a.c.do(ctx, http.MethodGet, "/api_keys", nil, &out); err != nil {
		return nil, err
	}
	keys := make([]apikeys.APIKey, 0, len(out.Keys))
	for _, d := range out.Keys {
		keys = append(keys, apikeys.APIKey{ID: d.ID, Name: d.Name, CreatedAt: d.CreatedAt, LastUsedAt: d.LastUsedAt})
	}
	return keys, nil
}

func (a *APIKeys) Delete(ctx context.Context, id int64) error {
	return a.c.do(ctx, http.MethodDelete, "/api_keys/"+strconv.FormatInt(id, 10), nil, nil)
}
