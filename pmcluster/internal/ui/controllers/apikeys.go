package controllers

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/pmapi"
)

// APIKeys lists API users (bearer tokens) and lets the operator create new
// ones. The bearer token is generated on the daemon and shown ONCE in the
// create response; listing never reveals token material.
type APIKeys struct{ *Controller }

type apiKeysData struct {
	Keys  []apiKeyRow
	Token string // one-time token from the last create, shown once
	Name  string // which user the one-time token belongs to
	Error string
	Msg   string
}

type apiKeyRow struct {
	ID        int64
	Name      string
	CreatedAt int64
}

func apiKeyRows(keys []pmapi.APIKey) []apiKeyRow {
	out := make([]apiKeyRow, 0, len(keys))
	for _, k := range keys {
		out = append(out, apiKeyRow{ID: k.ID, Name: k.Name, CreatedAt: k.CreatedAt})
	}
	return out
}

// Page renders the API keys page.
func (c APIKeys) Page(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := apiKeysData{}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "apikeys", d)
		return
	}
	d.Keys = c.fetch(ctx, &d)
	c.Views.Fragment(g, "apikeys", d)
}

// Add creates an API user and surfaces the one-time bearer token.
func (c APIKeys) Add(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := apiKeysData{}
	name := g.PostForm("name")

	switch {
	case !configured:
		d.Error = "pmcluster API not configured. Open Settings first."
	case name == "":
		d.Error = "Name is required."
	default:
		created, err := c.API.CreateAPIKey(ctx, name)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Name = created.Name
			d.Token = created.Token
			d.Msg = "API key for " + created.Name + " created. Copy the token now — it is shown once."
		}
	}

	d.Keys = c.fetch(ctx, &d)
	c.Views.Fragment(g, "apikeys", d)
}

// Remove deletes an API user by id, revoking its bearer token immediately.
func (c APIKeys) Remove(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := apiKeysData{}

	id, err := strconv.ParseInt(g.Param("id"), 10, 64)
	if configured && err == nil && id > 0 {
		if err := c.API.DeleteAPIKey(ctx, id); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Removed API key %d.", id)
		}
	} else if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
	} else {
		d.Error = "Invalid API key id."
	}

	d.Keys = c.fetch(ctx, &d)
	c.Views.Fragment(g, "apikeys", d)
}

// fetch lists API users, recording an error if the list itself fails.
func (c APIKeys) fetch(ctx context.Context, d *apiKeysData) []apiKeyRow {
	keys, err := c.API.ListAPIKeys(ctx)
	if err != nil {
		if d.Error == "" {
			d.Error = err.Error()
		}
		return nil
	}
	return apiKeyRows(keys)
}
