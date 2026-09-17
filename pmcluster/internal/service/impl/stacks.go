package impl

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Stacks is the read side of deployed stacks, backed by the store.
type Stacks struct {
	Store *store.Store
}

// NewStacks builds the local stacks adapter.
func NewStacks(st *store.Store) service.StacksService {
	return &Stacks{Store: st}
}

func (s *Stacks) Get(ctx context.Context, name string) (*store.Stack, error) {
	return s.Store.GetStack(ctx, name)
}

func (s *Stacks) List(ctx context.Context) ([]*store.Stack, error) {
	return s.Store.ListStacks(ctx)
}

func (s *Stacks) Revisions(ctx context.Context, name string, limit int) ([]*store.StackRevision, error) {
	return s.Store.ListRevisions(ctx, name, limit)
}
