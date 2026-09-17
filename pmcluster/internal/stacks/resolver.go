package stacks

import (
	"context"
	"errors"
	"fmt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// StoreConfigResolver implements manifest.EnvResolver against the DB config
// store, so `env: X: config(name)` values are injected from data.db.
type StoreConfigResolver struct {
	Store *store.Store
}

// ResolveConfig returns the DB config's content by name. Wraps store errors
// with operator-friendly context.
func (r *StoreConfigResolver) ResolveConfig(ctx context.Context, name string) (string, error) {
	if r == nil || r.Store == nil {
		return "", fmt.Errorf("config resolution unavailable (no store)")
	}
	c, err := r.Store.GetConfig(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrConfigNotFound) {
			return "", fmt.Errorf("config %q not found — create it with `pmcluster config create %s`", name, name)
		}
		return "", fmt.Errorf("get config %q: %w", name, err)
	}
	return c.Content, nil
}
