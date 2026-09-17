package remote

import (
	"context"
	"net/http"
	"net/url"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/webhooks"
)

// Webhooks is the HTTP adapter for webhooks.Service.
type Webhooks struct{ c *Client }

// NewWebhooks builds the remote webhooks adapter.
func NewWebhooks(c *Client) webhooks.Service { return &Webhooks{c: c} }

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

func (a *Webhooks) List(ctx context.Context) ([]webhooks.Source, error) {
	var out webhookListDTO
	if err := a.c.do(ctx, http.MethodGet, "/webhooks", nil, &out); err != nil {
		return nil, err
	}
	sources := make([]webhooks.Source, 0, len(out.Webhooks))
	for _, d := range out.Webhooks {
		sources = append(sources, webhooks.Source{
			Source:      d.Source,
			Description: d.Description,
			CreatedAt:   d.CreatedAt,
			LastUsedAt:  d.LastUsedAt,
		})
	}
	return sources, nil
}

func (a *Webhooks) Delete(ctx context.Context, source string) error {
	return a.c.do(ctx, http.MethodDelete, "/webhooks/"+url.PathEscape(source), nil, nil)
}
