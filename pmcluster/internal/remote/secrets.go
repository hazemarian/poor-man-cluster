package remote

import (
	"context"
	"net/http"
	"net/url"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/secrets"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Secrets is the HTTP adapter for secrets.Service. The payload (AES-GCM
// ciphertext) never crosses the wire; only the revealed value is fetched on
// demand.
type Secrets struct{ c *Client }

// NewSecrets builds the remote secrets adapter.
func NewSecrets(c *Client) secrets.Service { return &Secrets{c: c} }

type secretRowDTO struct {
	ID        int64  `json:"id"`
	Scope     string `json:"scope"`
	Stack     string `json:"stack,omitempty"`
	Name      string `json:"name"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
}

type secretListDTO struct {
	Secrets []secretRowDTO `json:"secrets"`
}

type secretValueDTO struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (a *Secrets) Create(ctx context.Context, scope, stack, name, value string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := a.c.do(ctx, http.MethodPost, "/secrets", map[string]string{
		"scope": scope,
		"stack": stack,
		"name":  name,
		"value": value,
	}, &out)
	return out.ID, err
}

// Get returns the secret's metadata row. The API has no dedicated metadata
// endpoint, so the row is looked up from the list.
func (a *Secrets) Get(ctx context.Context, name string) (*secrets.Secret, error) {
	rows, err := a.List(ctx, "", "")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Name == name {
			return &r, nil
		}
	}
	return nil, store.ErrSecretNotFound
}

func (a *Secrets) Reveal(ctx context.Context, name string) (string, error) {
	var out secretValueDTO
	if err := a.c.do(ctx, http.MethodGet, "/secrets/"+url.PathEscape(name)+"/value", nil, &out); err != nil {
		return "", err
	}
	return out.Value, nil
}

func (a *Secrets) List(ctx context.Context, scope, stack string) ([]secrets.Secret, error) {
	q := url.Values{}
	if scope != "" {
		q.Set("scope", scope)
	}
	if stack != "" {
		q.Set("stack", stack)
	}
	var out secretListDTO
	if err := a.c.do(ctx, http.MethodGet, "/secrets?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	rows := make([]secrets.Secret, 0, len(out.Secrets))
	for _, d := range out.Secrets {
		rows = append(rows, secrets.Secret{
			ID: d.ID, Scope: d.Scope, Stack: d.Stack, Name: d.Name,
			Hash: d.Hash, CreatedAt: d.CreatedAt,
		})
	}
	return rows, nil
}

func (a *Secrets) Update(ctx context.Context, name, value string) error {
	return a.c.do(ctx, http.MethodPut, "/secrets/"+url.PathEscape(name), map[string]string{
		"value": value,
	}, nil)
}

func (a *Secrets) Delete(ctx context.Context, name string) error {
	return a.c.do(ctx, http.MethodDelete, "/secrets/"+url.PathEscape(name), nil, nil)
}
