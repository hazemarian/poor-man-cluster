package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Nodes renders node mutations (promote/demote to storage node) backed by the
// daemon's /api/nodes/{hostname}/storage endpoints.
type Nodes struct{ *Controller }

// Promote marks a swarm node as a storage node (storage_nodes setting +
// pmcluster.storage label) via the daemon, then re-renders the overview
// fragment so the storage column/actions reflect the new state.
func (c Nodes) Promote(g *gin.Context) {
	hostname := g.Param("hostname")
	if hostname == "" {
		g.Status(http.StatusBadRequest)
		return
	}
	ctx := g.Request.Context()
	if err := c.API.PromoteNode(ctx, hostname); err != nil {
		g.Status(http.StatusBadGateway)
		return
	}
	c.overviewFragment(g)
}

// Demote removes a swarm node's storage role (storage_nodes setting + label
// clear) via the daemon, then re-renders the overview fragment.
func (c Nodes) Demote(g *gin.Context) {
	hostname := g.Param("hostname")
	if hostname == "" {
		g.Status(http.StatusBadRequest)
		return
	}
	ctx := g.Request.Context()
	if err := c.API.DemoteNode(ctx, hostname); err != nil {
		g.Status(http.StatusBadGateway)
		return
	}
	c.overviewFragment(g)
}

// overviewFragment re-renders the overview fragment after a node mutation.
func (c Nodes) overviewFragment(g *gin.Context) {
	ov := Overview(c)
	ov.Fragment(g)
}
