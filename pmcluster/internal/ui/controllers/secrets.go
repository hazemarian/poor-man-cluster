package controllers

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/pmapi"
)

// Secrets lists stored secrets (name/scope/hash — never payload) and lets the
// operator create new ones. The value is sent to the daemon once and stored
// AES-GCM encrypted; only the content hash is shown afterwards.
type Secrets struct{ *Controller }

type secretsData struct {
	Secrets    []secretRow
	Stack      string // when set (per-stack attach), prefill the create form
	RevealName string // when set, show the revealed plaintext for this secret
	RevealVal  string
	Msg        string
	Error      string
}

type secretRow struct {
	ID        int64
	Scope     string
	Name      string
	Hash      string
	CreatedAt int64
}

func secretRows(secs []pmapi.Secret) []secretRow {
	out := make([]secretRow, 0, len(secs))
	for _, s := range secs {
		out = append(out, secretRow{ID: s.ID, Scope: s.Scope, Name: s.Name, Hash: s.Hash, CreatedAt: s.CreatedAt})
	}
	return out
}

// Page renders the secrets page.
func (c Secrets) Page(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := secretsData{Stack: g.Query("stack")}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "secrets", d)
		return
	}
	d.Secrets = c.fetch(ctx, &d)
	c.Views.Fragment(g, "secrets", d)
}

// Add stores a new secret from the create form.
func (c Secrets) Add(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := secretsData{}

	name := g.PostForm("name")
	scope := g.PostForm("scope")
	value := g.PostForm("value")
	if scope == "" {
		scope = "service"
	}

	switch {
	case !configured:
		d.Error = "pmcluster API not configured. Open Settings first."
	case name == "":
		d.Error = "Name is required."
	case value == "":
		d.Error = "Value is required."
	default:
		_, err := c.API.CreateSecret(ctx, scope, name, value)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Secret %s created. The value is stored encrypted; only its hash is shown.", name)
		}
	}

	d.Secrets = c.fetch(ctx, &d)
	c.Views.Fragment(g, "secrets", d)
}

// Remove deletes a stored secret by name.
func (c Secrets) Remove(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := secretsData{}

	name := g.Param("name")
	switch {
	case !configured:
		d.Error = "pmcluster API not configured. Open Settings first."
	case name == "":
		d.Error = "Invalid secret name."
	default:
		if err := c.API.DeleteSecret(ctx, name); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Deleted secret %s.", name)
		}
	}

	d.Secrets = c.fetch(ctx, &d)
	c.Views.Fragment(g, "secrets", d)
}

// Reveal decrypts and shows a stored secret's plaintext on explicit request.
func (c Secrets) Reveal(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := secretsData{}
	name := g.Param("name")

	switch {
	case !configured:
		d.Error = "pmcluster API not configured. Open Settings first."
	case name == "":
		d.Error = "Invalid secret name."
	default:
		sv, err := c.API.RevealSecret(ctx, name)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.RevealName = sv.Name
			d.RevealVal = sv.Value
		}
	}

	d.Secrets = c.fetch(ctx, &d)
	c.Views.Fragment(g, "secrets", d)
}

// fetch lists stored secrets, recording an error if the list itself fails.
func (c Secrets) fetch(ctx context.Context, d *secretsData) []secretRow {
	secs, err := c.API.ListSecrets(ctx)
	if err != nil {
		if d.Error == "" {
			d.Error = err.Error()
		}
		return nil
	}
	return secretRows(secs)
}
