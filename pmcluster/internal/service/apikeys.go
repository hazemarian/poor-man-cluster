package service

import (
	"context"
	"errors"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// ErrEdgeUserProtected is returned by APIKeysService.Delete when the target
// is the 'edge' user that the operator console authenticates with.
var ErrEdgeUserProtected = errors.New("edge user is required by the operator console and cannot be removed")

// ErrSelfDelete is returned by APIKeysService.Delete when the caller tries
// to remove the API key it is currently authenticated with.
var ErrSelfDelete = errors.New("cannot remove the API key you are currently authenticated with")

// APIKeysService manages daemon API tokens (users). Tokens are shown once at
// creation; only a hash is stored.
type APIKeysService interface {
	Create(ctx context.Context, name string) (id int64, token string, err error)
	List(ctx context.Context) ([]store.UserRow, error)
	Delete(ctx context.Context, id int64) error
}
