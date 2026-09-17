package service

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// CredentialsService manages the platform bootstrap credentials (Traefik,
// Portainer, OpenObserve, edge) with rotation.
type CredentialsService interface {
	Get(ctx context.Context, name string) (*store.ManagedCredential, error)
	List(ctx context.Context) ([]*store.ManagedCredential, error)
	Reveal(ctx context.Context, name string) (string, error)
	Rotate(ctx context.Context, name string) (*cluster.ManagedCredential, error)
}
