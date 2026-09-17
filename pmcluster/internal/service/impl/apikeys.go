package impl

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// edgeAPITokenUser is the daemon user the operator console authenticates
// with; it must never be deletable.
const edgeAPITokenUser = "edge"

// APIKeys is the local adapter for the API-key (daemon user) use case.
type APIKeys struct {
	Store *store.Store
}

// NewAPIKeys wires the local API-key adapter.
func NewAPIKeys(st *store.Store) service.APIKeysService {
	return &APIKeys{Store: st}
}

// Create mints a pmc_<tokenID>_<secret> bearer token and stores only the
// token id plus an argon2id hash of the secret. The plaintext token is
// returned exactly once.
func (a *APIKeys) Create(ctx context.Context, name string) (int64, string, error) {
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
func (a *APIKeys) List(ctx context.Context) ([]store.UserRow, error) {
	return a.Store.ListUsers(ctx)
}

// Delete removes a daemon user by id. The operator-console user and the
// caller's own key are protected.
func (a *APIKeys) Delete(ctx context.Context, id int64) error {
	user, err := a.Store.UserByID(ctx, id)
	if err != nil {
		return err
	}
	if user.Name == edgeAPITokenUser {
		return service.ErrEdgeUserProtected
	}
	if me := auth.FromContext(ctx); me != nil && me.ID == id {
		return service.ErrSelfDelete
	}
	return a.Store.DeleteUser(ctx, id)
}
