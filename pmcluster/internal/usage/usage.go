// Package usage is the domain for the config/secret → stacks reference graph
// (GET /api/usage): which stacks reference which configs and secrets, computed
// from the latest rendered compose of each stack.
//
// Architecture: model + port here, local adapter in local.go (store-backed),
// HTTP handler in http.go, remote adapter in internal/remote/usage.go.
package usage

import (
	"context"
)

// Usage is the reference graph: config/secret name → stacks that reference it.
type Usage struct {
	Configs map[string][]string `json:"configs"`
	Secrets map[string][]string `json:"secrets"`
}

// Service is the port for computing the usage graph.
type Service interface {
	// Get computes which stacks reference which configs and secrets.
	Get(ctx context.Context) (*Usage, error)
}
