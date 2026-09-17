package service

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// WebhooksService manages the webhook receiver sources. Each source has a
// one-time secret used to HMAC-sign deploy requests.
type WebhooksService interface {
	Create(ctx context.Context, source, description string) (secret string, err error)
	List(ctx context.Context) ([]*store.WebhookSource, error)
	Delete(ctx context.Context, source string) error
}
