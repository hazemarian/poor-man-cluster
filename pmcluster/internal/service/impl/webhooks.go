package impl

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Webhooks is the local adapter for the webhook-source use case. The one-time
// secret is generated here and stored AES-256-GCM encrypted, exactly like the
// daemon does for its own webhook credentials.
type Webhooks struct {
	Store  *store.Store
	Cipher *credentials.Cipher
}

// NewWebhooks wires the local webhook-source adapter.
func NewWebhooks(st *store.Store, c *credentials.Cipher) service.WebhooksService {
	return &Webhooks{Store: st, Cipher: c}
}

// Create generates a 32-byte random hex secret, stores it encrypted and
// returns the plaintext secret exactly once.
func (w *Webhooks) Create(ctx context.Context, source, description string) (string, error) {
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", err
	}
	secretHex := hex.EncodeToString(secretBytes)
	ciphertext, err := w.Cipher.Encrypt([]byte(secretHex))
	if err != nil {
		return "", err
	}
	if err := w.Store.CreateWebhookSource(ctx, source, description, ciphertext); err != nil {
		return "", err
	}
	return secretHex, nil
}

// List returns every stored webhook source.
func (w *Webhooks) List(ctx context.Context) ([]*store.WebhookSource, error) {
	return w.Store.ListWebhookSources(ctx)
}

// Delete removes a webhook source by name.
func (w *Webhooks) Delete(ctx context.Context, source string) error {
	return w.Store.DeleteWebhookSource(ctx, source)
}
