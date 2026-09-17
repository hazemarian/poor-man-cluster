// Package webhooks is the bounded context for the HMAC-verified deploy
// webhook receiver and its source registry.
//
// A Source is one named CI/automation integration allowed to POST to
// /webhook/{source}. Each source holds a one-time shared secret, stored
// AES-256-GCM encrypted at rest and returned in plaintext exactly once.
// The management surface (Service) is served by both the local adapter and
// the remote REST client; the receiver additionally needs the decrypted
// secret material, which is deliberately local-only (SourceReader) — the
// secret never crosses the wire.
package webhooks

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/stacks"
)

// Source is one configured webhook source. Description is empty when unset;
// LastUsedAt is 0 when the source has never fired.
type Source struct {
	Source      string
	Description string
	CreatedAt   int64
	LastUsedAt  int64
}

// Service manages webhook sources. The shared secret returned by Create is
// shown to the caller exactly once; listing never reveals it.
type Service interface {
	Create(ctx context.Context, source, description string) (secret string, err error)
	List(ctx context.Context) ([]Source, error)
	Delete(ctx context.Context, source string) error
}

// SourceReader provides the receiver with the decrypted secret material for
// HMAC verification. Local-only by design — the secret never leaves the node.
type SourceReader interface {
	Secret(ctx context.Context, source string) ([]byte, error)
	MarkUsed(ctx context.Context, source string) error
}

// Deployer is the narrow deploy surface the receiver needs: validate a
// payload and deploy it. Satisfied by stacks.Service (the stacks domain's
// Deployer port).
type Deployer interface {
	Deploy(ctx context.Context, p stacks.Payload) (*stacks.Result, error)
}
