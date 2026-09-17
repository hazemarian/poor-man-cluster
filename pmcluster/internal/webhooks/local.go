package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Local is the on-node adapter for the webhook domain. It implements both
// Service (management) and SourceReader (secret material for the receiver).
type Local struct {
	Store  *store.Store
	Cipher *credentials.Cipher
}

// NewLocal wires the local webhook adapter.
func NewLocal(st *store.Store, c *credentials.Cipher) *Local {
	return &Local{Store: st, Cipher: c}
}

// Create generates a 32-byte random hex secret, stores it encrypted and
// returns the plaintext secret exactly once.
func (w *Local) Create(ctx context.Context, source, description string) (string, error) {
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
func (w *Local) List(ctx context.Context) ([]Source, error) {
	rows, err := w.Store.ListWebhookSources(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Source, 0, len(rows))
	for _, r := range rows {
		s := Source{Source: r.Source, CreatedAt: r.CreatedAt}
		if r.Description.Valid {
			s.Description = r.Description.String
		}
		if r.LastUsedAt.Valid {
			s.LastUsedAt = r.LastUsedAt.Int64
		}
		out = append(out, s)
	}
	return out, nil
}

// Delete removes a webhook source by name.
func (w *Local) Delete(ctx context.Context, source string) error {
	return w.Store.DeleteWebhookSource(ctx, source)
}

// Secret returns the decrypted shared secret for the source.
func (w *Local) Secret(ctx context.Context, source string) ([]byte, error) {
	src, err := w.Store.GetWebhookSource(ctx, source)
	if err != nil {
		return nil, err
	}
	return w.Cipher.Decrypt(src.SecretCiphertext)
}

// MarkUsed records that the source fired at the current time (best-effort).
func (w *Local) MarkUsed(ctx context.Context, source string) error {
	return w.Store.MarkWebhookSourceUsed(ctx, source)
}
