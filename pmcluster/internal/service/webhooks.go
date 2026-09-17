package service

import "github.com/hazemarian/poor-man-stack/pmcluster/internal/webhooks"

// WebhooksService is an alias for the webhooks domain port. Kept so existing
// consumers compile during the DDD restructure.
type WebhooksService = webhooks.Service
