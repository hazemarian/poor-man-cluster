package controllers

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/pmapi"
)

// secretRow is a stored secret as shown in the UI — payload never leaves the
// daemon; only the content hash is displayed. Scope "cluster" secrets live in
// Settings; scope "service" secrets belong to the stack named in Stack.
type secretRow struct {
	ID        int64
	Scope     string
	Stack     string
	Name      string
	Hash      string
	CreatedAt int64
}

func secretRows(secs []pmapi.Secret) []secretRow {
	out := make([]secretRow, 0, len(secs))
	for _, s := range secs {
		out = append(out, secretRow{ID: s.ID, Scope: s.Scope, Stack: s.Stack, Name: s.Name, Hash: s.Hash, CreatedAt: s.CreatedAt})
	}
	return out
}

// listSecretsFor lists stored secrets filtered by scope ("" = all) and stack
// ("" = all).
func (c *Controller) listSecretsFor(ctx context.Context, scope, stack string) ([]secretRow, error) {
	secs, err := c.API.ListSecrets(ctx, scope, stack)
	if err != nil {
		return nil, err
	}
	return secretRows(secs), nil
}
