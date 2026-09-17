package service

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// StacksService is the read side of deployed stacks.
type StacksService interface {
	Get(ctx context.Context, name string) (*store.Stack, error)
	List(ctx context.Context) ([]*store.Stack, error)
	Revisions(ctx context.Context, name string, limit int) ([]*store.StackRevision, error)
}
