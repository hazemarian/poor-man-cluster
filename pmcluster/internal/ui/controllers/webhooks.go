package controllers

import (
	"context"
	"net/url"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/pmapi"
)

// Webhooks lists deploy-webhook sources and lets the operator add/remove them.
// The HMAC shared secret is generated on the daemon and shown ONCE in the
// create response; listing never reveals it.
type Webhooks struct{ *Controller }

type webhooksData struct {
	Sources    []webhookRow
	Deliveries []deliveryRow
	Source     string
	Secret     string
	Error      string
	Msg        string
}

// webhookRow carries raw epoch int64 timestamps; the template renders them via
// the {{ts}} helper.
type webhookRow struct {
	Source      string
	Description string
	CreatedAt   int64
	LastUsedAt  int64
}

// deliveryRow is one recorded webhook delivery attempt, rendered in the
// per-source history table.
type deliveryRow struct {
	ID        int64
	Status    string
	StackName string
	Revision  int64
	RepoURL   string
	File      string
	Error     string
	CreatedAt int64
}

func deliveryRows(ds []pmapi.WebhookDelivery) []deliveryRow {
	out := make([]deliveryRow, 0, len(ds))
	for _, d := range ds {
		out = append(out, deliveryRow{
			ID:        d.ID,
			Status:    d.Status,
			StackName: d.StackName,
			Revision:  d.Revision,
			RepoURL:   d.RepoURL,
			File:      d.File,
			Error:     d.Error,
			CreatedAt: d.CreatedAt,
		})
	}
	return out
}

func webhookRows(srcs []pmapi.Webhook) []webhookRow {
	out := make([]webhookRow, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, webhookRow{
			Source:      s.Source,
			Description: s.Description,
			CreatedAt:   s.CreatedAt,
			LastUsedAt:  s.LastUsedAt,
		})
	}
	return out
}

// Page renders the webhook sources page.
func (c Webhooks) Page(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := webhooksData{}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "webhooks", d)
		return
	}
	d.Sources = c.fetch(ctx, &d)
	c.Views.Fragment(g, "webhooks", d)
}

// Add creates a webhook source and surfaces the one-time shared secret.
func (c Webhooks) Add(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := webhooksData{}
	source := g.PostForm("source")
	desc := g.PostForm("description")

	switch {
	case !configured:
		d.Error = "pmcluster API not configured. Open Settings first."
	case source == "":
		d.Error = "Source name is required."
	default:
		created, err := c.API.CreateWebhook(ctx, source, desc)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Source = created.Source
			d.Secret = created.Secret
			d.Msg = "Webhook source " + created.Source + " created. Copy the shared secret now — it is shown once."
		}
	}

	d.Sources = c.fetch(ctx, &d)
	c.Views.Fragment(g, "webhooks", d)
}

// Remove deletes a webhook source, revoking its shared secret immediately.
func (c Webhooks) Remove(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := webhooksData{}
	source, err := url.PathUnescape(g.Param("source"))
	if err != nil {
		source = g.Param("source")
	}

	if configured && source != "" {
		if err := c.API.DeleteWebhook(ctx, source); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = "Removed webhook source " + source + "."
		}
	} else if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
	}

	d.Sources = c.fetch(ctx, &d)
	c.Views.Fragment(g, "webhooks", d)
}

// fetch lists webhook sources, recording an error if the list itself fails.
func (c Webhooks) fetch(ctx context.Context, d *webhooksData) []webhookRow {
	srcs, err := c.API.ListWebhooks(ctx)
	if err != nil {
		if d.Error == "" {
			d.Error = err.Error()
		}
		return nil
	}
	return webhookRows(srcs)
}

// Deliveries renders the recorded delivery history for one source, newest
// first (limit clamped to the daemon page size).
func (c Webhooks) Deliveries(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := webhooksData{Source: g.Param("source")}
	if configured && d.Source != "" {
		ds, err := c.API.ListWebhookDeliveries(ctx, d.Source, 50)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Deliveries = deliveryRows(ds)
		}
	} else if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
	}
	c.Views.Fragment(g, "webhookdeliveries", d)
}
