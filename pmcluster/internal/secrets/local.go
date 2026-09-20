package secrets

import (
	"context"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Local implements Service against the local store and the AES-GCM cipher.
// Values are encrypted before storage and decrypted only for Reveal; a sha256
// hash is stored alongside for verification.
type Local struct {
	Store  *store.Store
	Cipher *credentials.Cipher
}

// NewLocal wires the secrets service onto the store and cipher.
func NewLocal(st *store.Store, c *credentials.Cipher) *Local {
	return &Local{Store: st, Cipher: c}
}

func (s *Local) Create(ctx context.Context, scope, stack, name, value string) (int64, error) {
	payload, err := s.Cipher.Encrypt([]byte(value))
	if err != nil {
		return 0, err
	}
	return s.Store.CreateSecret(ctx, scope, stack, name, payload, store.SecretHash(value))
}

func (s *Local) Get(ctx context.Context, name string) (*Secret, error) {
	row, err := s.Store.GetSecret(ctx, name)
	if err != nil {
		return nil, err
	}
	return &Secret{ID: row.ID, Scope: row.Scope, Stack: row.Stack, Name: row.Name, Hash: row.Hash, CreatedAt: row.CreatedAt}, nil
}

func (s *Local) Reveal(ctx context.Context, name string) (string, error) {
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

func (s *Local) List(ctx context.Context, scope, stack string) ([]Secret, error) {
	rows, err := s.Store.ListSecrets(ctx, scope, stack)
	if err != nil {
		return nil, err
	}
	out := make([]Secret, 0, len(rows))
	for _, r := range rows {
		out = append(out, Secret{ID: r.ID, Scope: r.Scope, Stack: r.Stack, Name: r.Name, Hash: r.Hash, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

func (s *Local) Update(ctx context.Context, name, value string) error {
	payload, err := s.Cipher.Encrypt([]byte(value))
	if err != nil {
		return err
	}
	return s.Store.UpdateSecret(ctx, name, payload, store.SecretHash(value))
}

func (s *Local) Delete(ctx context.Context, name string) error {
	return s.Store.DeleteSecret(ctx, name)
}
