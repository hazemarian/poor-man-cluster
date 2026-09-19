// Package apikeys is the bounded context for daemon API tokens (users):
// minting pmc_ bearer tokens, listing users and revoking keys. Tokens are
// shown once at creation; only the token id and an argon2id hash are stored.
package apikeys

import (
	"context"
	"errors"
)

// ErrEdgeUserProtected is returned by Service.Delete when the target is the
// 'edge' user that the operator console authenticates with.
var ErrEdgeUserProtected = errors.New("edge user is required by the operator console and cannot be removed")

// ErrSelfDelete is returned by Service.Delete when the caller tries to remove
// the API key it is currently authenticated with.
var ErrSelfDelete = errors.New("cannot remove the API key you are currently authenticated with")

// APIKey is a daemon bearer-token user. The token itself is never stored;
// only its id and an argon2id hash of the secret are kept.
type APIKey struct {
	ID         int64
	Name       string
	CreatedAt  int64
	LastUsedAt int64
}

// Service manages daemon API tokens (users). Tokens are shown once at
// creation; only a hash is stored.
type Service interface {
	// Create mints a pmc_<tokenID>_<secret> token, stores the token id plus
	// an argon2id hash, and returns the plaintext token exactly once.
	Create(ctx context.Context, name string) (id int64, token string, err error)
	// List returns all users without any token material.
	List(ctx context.Context) ([]APIKey, error)
	// Delete revokes a user by id. The 'edge' console user and the caller's
	// own key are protected (ErrEdgeUserProtected / ErrSelfDelete).
	Delete(ctx context.Context, id int64) error
}
