package impl

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Secrets implements service.SecretsService against the local store and the
// AES-GCM cipher. Values are encrypted before storage and decrypted only for
// Reveal; a sha256 hash is stored alongside for verification.
type Secrets struct {
	Store  *store.Store
	Cipher *credentials.Cipher
}

// NewSecrets wires the secrets service onto the store and cipher.
func NewSecrets(st *store.Store, c *credentials.Cipher) service.SecretsService {
	return &Secrets{Store: st, Cipher: c}
}

func (s *Secrets) Create(ctx context.Context, scope, stack, name, value string) (int64, error) {
	payload, err := s.Cipher.Encrypt([]byte(value))
	if err != nil {
		return 0, err
	}
	return s.Store.CreateSecret(ctx, scope, stack, name, payload, store.SecretHash(value))
}

func (s *Secrets) Get(ctx context.Context, name string) (*store.SecretRow, error) {
	return s.Store.GetSecret(ctx, name)
}

func (s *Secrets) Reveal(ctx context.Context, name string) (string, error) {
	sec, err := s.Store.GetSecret(ctx, name)
	if err != nil {
		return "", err
	}
	plain, err := s.Cipher.Decrypt(sec.Payload)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (s *Secrets) List(ctx context.Context, scope, stack string) ([]*store.SecretRow, error) {
	return s.Store.ListSecrets(ctx, scope, stack)
}

func (s *Secrets) Update(ctx context.Context, name, value string) error {
	payload, err := s.Cipher.Encrypt([]byte(value))
	if err != nil {
		return err
	}
	return s.Store.UpdateSecret(ctx, name, payload, store.SecretHash(value))
}

func (s *Secrets) Delete(ctx context.Context, name string) error {
	return s.Store.DeleteSecret(ctx, name)
}
