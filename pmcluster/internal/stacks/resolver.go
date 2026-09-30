package stacks

import (
	"context"
	"errors"
	"fmt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// StoreConfigResolver implements manifest.EnvResolver against the DB config
// store, so `env: X: config(name)` values are injected from data.db.
//
// Resolutions are STACK-SCOPED: each lookup names the stack being deployed,
// and only that stack's own rows (or unattached shared rows with an empty
// stack) are eligible. A config created for a different stack is never
// resolved — it would otherwise silently leak into another stack's env.
type StoreConfigResolver struct {
	Store *store.Store
}

// ResolveConfig returns the DB config's content for the given stack, scoped
// to that stack (or a shared empty-stack row). Wraps store errors with
// operator-friendly context.
func (r *StoreConfigResolver) ResolveConfig(ctx context.Context, stack, name string) (string, error) {
	if r == nil || r.Store == nil {
		return "", fmt.Errorf("config resolution unavailable (no store)")
	}
	c, err := r.Store.GetConfigForStack(ctx, stack, name)
	if err != nil {
		if errors.Is(err, store.ErrConfigNotFound) {
			return "", fmt.Errorf("config %q not found for stack %q — create it with `pmcluster config create --scope service --stack %s %s`, or leave the stack empty to share it across stacks", name, stack, stack, name)
		}
		return "", fmt.Errorf("get config %q for stack %q: %w", name, stack, err)
	}
	return c.Content, nil
}
