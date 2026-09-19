package apikeys

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// edgeAPITokenUser is the daemon user the operator console authenticates
// with; it must never be deletable.
const edgeAPITokenUser = "edge"

// Local is the on-node adapter backed by the SQLite store.
type Local struct {
	Store *store.Store
}

// NewLocal wires the local api-keys adapter.
func NewLocal(st *store.Store) Service {
	return &Local{Store: st}
}

// Create mints a pmc_<tokenID>_<secret> bearer token and stores only the
// token id plus an argon2id hash of the secret. The plaintext token is
// returned exactly once.
func (a *Local) Create(ctx context.Context, name string) (int64, string, error) {
	token, _ := auth.GenerateToken()
	tokenID, secret := auth.SplitToken(token)
	hash, _ := auth.HashToken(secret)
	id, err := a.Store.CreateUser(ctx, name, tokenID, hash)
	if err != nil {
		return 0, "", err
	}
	return id, token, nil
}

// List returns all daemon users (API keys).
func (a *Local) List(ctx context.Context) ([]APIKey, error) {
	rows, err := a.Store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]APIKey, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, APIKey{ID: r.ID, Name: r.Name, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt})
	}
	return keys, nil
}

// Delete removes a daemon user by id. The operator-console user and the
// caller's own key are protected.
func (a *Local) Delete(ctx context.Context, id int64) error {
	user, err := a.Store.UserByID(ctx, id)
	if err != nil {
		return err
	}
	if user.Name == edgeAPITokenUser {
		return ErrEdgeUserProtected
	}
	if me := auth.FromContext(ctx); me != nil && me.ID == id {
		return ErrSelfDelete
	}
	return a.Store.DeleteUser(ctx, id)
}
