package service

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// SecretsService is the business entry point for the encrypted secrets store.
// Values are stored AES-GCM encrypted; only a hash is ever listed, and the
// plaintext is returned only on demand (Reveal). The local adapter wraps
// *store.Store plus the encryption cipher.
type SecretsService interface {
	Create(ctx context.Context, scope, stack, name, value string) (int64, error)
	// Get returns a secret's metadata row (hash, scope, timestamps; no payload).
	Get(ctx context.Context, name string) (*store.SecretRow, error)
	// Reveal returns the decrypted value for a secret name.
	Reveal(ctx context.Context, name string) (string, error)
	List(ctx context.Context, scope, stack string) ([]*store.SecretRow, error)
	// Update replaces a stored value in place.
	Update(ctx context.Context, name, value string) error
	Delete(ctx context.Context, name string) error
}
