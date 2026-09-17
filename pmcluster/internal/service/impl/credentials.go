package impl

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// credentialRotator rotates a managed credential end-to-end (credential row,
// swarm secret and the daemon secrets row). cluster.CredentialsManager
// satisfies it; the CLI composes it with its Docker client and deployer.
type credentialRotator interface {
	Rotate(ctx context.Context, name string) (*cluster.ManagedCredential, error)
}

// Credentials is the local adapter for the CredentialsService port. It reads
// encrypted rows from the store, reveals plaintext via the encryption key, and
// delegates rotation to the injected rotator (nil disables rotation).
type Credentials struct {
	Store   *store.Store
	Cipher  *credentials.Cipher
	Rotator credentialRotator
}

// NewCredentials builds the local CredentialsService adapter.
func NewCredentials(st *store.Store, cipher *credentials.Cipher, rotator credentialRotator) service.CredentialsService {
	return &Credentials{Store: st, Cipher: cipher, Rotator: rotator}
}

// Get returns the stored credential row (password ciphertext only).
func (c *Credentials) Get(ctx context.Context, name string) (*store.ManagedCredential, error) {
	return c.Store.GetCredential(ctx, name)
}

// List returns all managed credentials.
func (c *Credentials) List(ctx context.Context) ([]*store.ManagedCredential, error) {
	return c.Store.ListCredentials(ctx)
}

// Reveal decrypts the credential's password.
func (c *Credentials) Reveal(ctx context.Context, name string) (string, error) {
	if c.Cipher == nil {
		return "", fmt.Errorf("encryption key unavailable")
	}
	cred, err := c.Store.GetCredential(ctx, name)
	if err != nil {
		return "", err
	}
	plain, err := c.Cipher.Decrypt(cred.PasswordCiphertext)
	if err != nil {
		return "", fmt.Errorf("decrypt credential %s: %w", name, err)
	}
	return string(plain), nil
}

// Rotate delegates to the injected rotator (the CLI composes
// cluster.CredentialsManager with Docker + deployer).
func (c *Credentials) Rotate(ctx context.Context, name string) (*cluster.ManagedCredential, error) {
	if c.Rotator == nil {
		return nil, fmt.Errorf("credential rotation not configured")
	}
	return c.Rotator.Rotate(ctx, name)
}
