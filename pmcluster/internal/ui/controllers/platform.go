package controllers

import (
	"github.com/gin-gonic/gin"
)

// Platform lists the platform-managed swarm services (edge, ingress,
// observability, backup, SSO). They are kept apart from customer app services
// because they are driven by the platform's DB configs (not `stacks` rows), so
// the Services page and the stack pages only ever show customer apps. The
// discriminator is the io.pmcluster.platform=true label the compose writer
// stamps on every platform service — surfaced as pmapi.Service.Platform.
type Platform struct{ *Controller }

type platformData struct {
	Services []serviceRow
	// Known separates "the list was read and is empty" from "the read failed":
	// the second one is never rendered as zero services.
	Known  bool
	Count  int64
	Stacks int64 // distinct platform stacks owning at least one service

	ErrKey string
	ErrRaw string
}

// List renders the platform services page.
func (c Platform) List(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := platformData{}
	if !configured {
		d.ErrKey = errKeyNotConfigured
		c.Views.Fragment(g, "platform", d)
		return
	}
	svcs, err := c.API.ListServices(ctx, "")
	if err != nil {
		d.ErrKey, d.ErrRaw = humanErr("err.services", err)
		c.Views.Fragment(g, "platform", d)
		return
	}
	d.Known = true
	stacks := map[string]bool{}
	for _, s := range svcs {
		if !s.Platform {
			continue
		}
		d.Services = append(d.Services, newServiceRow(s))
		if s.Stack != "" {
			stacks[s.Stack] = true
		}
	}
	d.Count = int64(len(d.Services))
	d.Stacks = int64(len(stacks))
	c.Views.Fragment(g, "platform", d)
}
