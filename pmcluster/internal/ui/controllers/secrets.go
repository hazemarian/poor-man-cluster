package controllers

import (
	"context"

	"github.com/gin-gonic/gin"

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

// secretFormData drives the shared secret create/edit modal fragment
// ("secretform"). Action is the POST endpoint the form submits to. The stored
// value is never shown in the form — editing replaces it with a new value.
type secretFormData struct {
	Scope  string // cluster | service
	Stack  string // owning stack (service scope)
	Name   string
	Value  string
	IsEdit bool
	Action string
	Error  string
}

// secretForm renders the secret modal fragment. Handlers use it for the
// hx-get "new"/"edit" routes (plain 200 → openModal) and for validation
// failures (422 + HX-Retarget so the error stays inside the modal).
func (c *Controller) secretForm(g *gin.Context, d secretFormData) {
	c.Views.Fragment(g, "secretform", d)
}

// secretRevealData drives the reveal modal fragment ("secretreveal") — the
// value is shown on demand with a confirmation prompt, never in list copy.
type secretRevealData struct {
	Name  string
	Value string
	Error string
}
