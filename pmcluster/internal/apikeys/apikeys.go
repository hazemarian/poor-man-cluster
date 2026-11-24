// Package apikeys is the bounded context for daemon API tokens (users):
// minting pmc_ bearer tokens, listing users and revoking keys. Tokens are
// shown once at creation; only the token id and an argon2id hash are stored.
package apikeys

import (
	"context"
	"errors"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

// Key kinds. A "mcp" key is a tool key: its whole surface is /api/mcp, and the
// REST control plane refuses it. That is the point — an AI client gets the
// tools, not the ability to deploy.
const (
	KindAPI = "api"
	KindMCP = "mcp"
)

// ErrInvalidRole / ErrInvalidKind reject a key that names a tier or a surface
// nobody defined, rather than silently minting the default.
var (
	ErrInvalidRole = errors.New("role must be one of admin, operator, viewer")
	ErrInvalidKind = errors.New("kind must be one of api, mcp")
)

// ValidRole reports whether role is a daemon tier.
func ValidRole(role string) bool {
	switch role {
	case auth.RoleAdmin, auth.RoleOperator, auth.RoleViewer:
		return true
	}
	return false
}

// ValidKind reports whether kind is a key surface.
func ValidKind(kind string) bool {
	return kind == KindAPI || kind == KindMCP
}

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
	// Stack is the optional per-token stack scope: a non-empty value binds
	// the token to that one application stack ("" = unscoped).
	Stack string
	// Role is the daemon tier: admin (a master key — everything, including the
	// secret-bearing surfaces), operator (deploy and operate, no secrets) or
	// viewer (reads).
	Role string
	// Kind is the surface: "api" for the REST control plane, "mcp" for a key
	// that may only call the MCP endpoint.
	Kind string
}

// Service manages daemon API tokens (users). Tokens are shown once at
// creation; only a hash is stored.
type Service interface {
	// Create mints a pmc_<tokenID>_<secret> token, stores the token id plus
	// an argon2id hash, and returns the plaintext token exactly once.
	//
	// stack scopes the token to one application stack ("" = every stack).
	// role is the daemon tier ("" = operator, the least privileged tier that can
	// still operate a stack). kind is the surface ("" = api).
	Create(ctx context.Context, name, stack, role, kind string) (id int64, token string, err error)
	// List returns all users without any token material.
	List(ctx context.Context) ([]APIKey, error)
	// Delete revokes a user by id. The 'edge' console user and the caller's
	// own key are protected (ErrEdgeUserProtected / ErrSelfDelete).
	Delete(ctx context.Context, id int64) error
}
