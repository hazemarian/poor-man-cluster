package controllers

import (
	"github.com/gin-gonic/gin"
)

// Usage shows which stacks reference which configs and secrets, computed by
// the daemon from the latest rendered compose of each stack (GET /api/usage).
type Usage struct{ *Controller }

type usageData struct {
	Configs map[string][]string
	Secrets map[string][]string
	Error   string
}

// Page renders the usage graph (viewer role).
func (c Usage) Page(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := usageData{}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "usage", d)
		return
	}
	u, err := c.API.GetUsage(ctx)
	if err != nil {
		d.Error = err.Error()
		c.Views.Fragment(g, "usage", d)
		return
	}
	d.Configs = u.Configs
	d.Secrets = u.Secrets
	c.Views.Fragment(g, "usage", d)
}
