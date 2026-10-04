// Package secrets is the bounded context for the encrypted application
// secrets store. Values are AES-GCM encrypted at rest; only a sha256 hash is
// ever listed, and the plaintext is returned on demand (Reveal) or revealed
// once at creation.
package secrets

import "context"

// Secret is the metadata view of a stored secret. The ciphertext payload is
// never exposed through this model — the plaintext only ever appears via
// Reveal.
type Secret struct {
	ID        int64
	Scope     string
	Stack     string
	Name      string
	Hash      string
	CreatedAt int64
	// SwarmRev is the rotation count (1 = first value). The CLI mirrors the
	// value into the swarm secret named store.SwarmSecretName(name, rev),
	// keeping rotations out of the immutable in-use swarm secret (BUG-007).
	SwarmRev int64
}

// Service is the business entry point for the secrets store. Get and List
// return metadata only; Reveal decrypts on demand.
type Service interface {
	Create(ctx context.Context, scope, stack, name, value string) (int64, error)
	// Get returns a secret's metadata row (hash, scope, timestamps; no payload).
	Get(ctx context.Context, name string) (*Secret, error)
	// Reveal returns the decrypted value for a secret name.
	Reveal(ctx context.Context, name string) (string, error)
	List(ctx context.Context, scope, stack string) ([]Secret, error)
	// Update replaces a stored value in place.
	Update(ctx context.Context, name, value string) error
	// Retag reassigns a secret's scope and stack (e.g. bind an accidentally
	// unattached row to a stack). Returns ErrSecretNotFound when missing.
	Retag(ctx context.Context, name, scope, stack string) error
	Delete(ctx context.Context, name string) error
}
