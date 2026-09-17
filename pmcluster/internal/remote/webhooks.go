package remote

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Webhooks is the HTTP adapter for service.WebhooksService.
type Webhooks struct{ c *Client }

// NewWebhooks builds the remote webhooks adapter.
func NewWebhooks(c *Client) service.WebhooksService { return &Webhooks{c: c} }

type webhookDTO struct {
	Source      string `json:"source"`
	Description string `json:"description,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	LastUsedAt  int64  `json:"last_used_at,omitempty"`
}

type webhookListDTO struct {
	Webhooks []webhookDTO `json:"webhooks"`
}

type webhookCreatedDTO struct {
	Source string `json:"source"`
	Secret string `json:"secret"`
}

func (a *Webhooks) Create(ctx context.Context, source, description string) (string, error) {
	var out webhookCreatedDTO
	err := a.c.do(ctx, http.MethodPost, "/webhooks", map[string]string{
		"source":      source,
		"description": description,
	}, &out)
	return out.Secret, err
}

func (a *Webhooks) List(ctx context.Context) ([]*store.WebhookSource, error) {
	var out webhookListDTO
	if err := a.c.do(ctx, http.MethodGet, "/webhooks", nil, &out); err != nil {
		return nil, err
	}
	sources := make([]*store.WebhookSource, 0, len(out.Webhooks))
	for _, d := range out.Webhooks {
		var desc sql.NullString
		if d.Description != "" {
			desc = sql.NullString{String: d.Description, Valid: true}
		}
		var used sql.NullInt64
		if d.LastUsedAt != 0 {
			used = sql.NullInt64{Int64: d.LastUsedAt, Valid: true}
		}
		sources = append(sources, &store.WebhookSource{
			Source:      d.Source,
			Description: desc,
			CreatedAt:   d.CreatedAt,
			LastUsedAt:  used,
		})
	}
	return sources, nil
}

func (a *Webhooks) Delete(ctx context.Context, source string) error {
	return a.c.do(ctx, http.MethodDelete, "/webhooks/"+url.PathEscape(source), nil, nil)
}
