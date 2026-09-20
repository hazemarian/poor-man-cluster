package controllers

import (
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// Webhooks lists deploy-webhook sources and lets the operator add and revoke
// them. The HMAC shared secret is generated on the daemon and returned ONCE in
// the create response; listing never reveals it.
type Webhooks struct{ *Controller }

// webhooksData is the view model of frag_webhooks.html.
//
// Known is the difference between "the daemon answered with no sources" (an
// actionable empty list) and "the list call failed" (unknown): a failed read
// must never render as an empty table, because an empty table says the cluster
// accepts no webhooks, which is a lie.
type webhooksData struct {
	Sources      []webhookRow
	Known        bool
	Configured   bool
	Count        int
	Base         string // public origin of the receiver, "" when no domain is set
	Secret       string
	SecretSource string
	SecretEndpt  string
	DomainKnown  bool
	Form         webhookForm
	ErrKey       string
	ErrRaw       string
	MsgKey       string
	MsgArg       string

	// Source names the webhook source whose delivery history is open
	// (frag_webhookdeliveries.html); empty everywhere else.
	Source string

	// Per-source delivery history (frag_webhookdeliveries.html).
	Deliveries      []deliveryRow
	DeliveriesKnown bool
	DeliveryLimit   int64
}

type webhookForm struct {
	Source      string
	Description string
}

// webhookRow carries raw epoch int64 timestamps; the template renders them via
// the {{ts}} helper. Endpoint is the absolute receiver URL when the console
// knows its public domain, so the row can offer it for copying.
type webhookRow struct {
	Source      string
	Description string
	CreatedAt   int64
	LastUsedAt  int64
	Endpoint    string
}

// endpointBase is the public origin the receiver answers on, or "" when the
// console was started without a cluster domain — in which case no page can
// invent a URL and the templates say so instead.
func (c Webhooks) endpointBase() string {
	domain := strings.TrimSpace(c.Domain)
	if domain == "" {
		return ""
	}
	if strings.Contains(domain, "://") {
		return strings.TrimSuffix(domain, "/")
	}
	return "https://" + strings.TrimSuffix(domain, "/")
}

func endpointFor(base, source string) string {
	if base == "" {
		return ""
	}
	return base + "/webhook/" + url.PathEscape(source)
}

// Page renders the webhook sources page.
func (c Webhooks) Page(g *gin.Context) {
	d := webhooksData{
		Configured:  c.API.Configured(),
		DomainKnown: c.Domain != "",
	}
	c.load(g, &d)
	c.Views.Fragment(g, "webhooks", d)
}

// Add creates a webhook source and surfaces the one-time shared secret.
func (c Webhooks) Add(g *gin.Context) {
	ctx := g.Request.Context()
	d := webhooksData{
		Configured:  c.API.Configured(),
		DomainKnown: c.Domain != "",
		Form: webhookForm{
			Source:      g.PostForm("source"),
			Description: g.PostForm("description"),
		},
	}

	switch {
	case !d.Configured:
		d.ErrKey = "err.api_not_configured"
	case strings.TrimSpace(d.Form.Source) == "":
		d.ErrKey = "webhooks.err_source"
	default:
		created, err := c.API.CreateWebhook(ctx, d.Form.Source, d.Form.Description)
		if err != nil {
			d.ErrKey, d.ErrRaw = "webhooks.err_create", err.Error()
		} else {
			d.Secret = created.Secret
			d.SecretSource = created.Source
			d.SecretEndpt = endpointFor(c.endpointBase(), created.Source)
			d.MsgKey, d.MsgArg = "webhooks.msg_created", created.Source
		}
	}

	c.load(g, &d)
	c.Views.Fragment(g, "webhooks", d)
}

// Remove deletes a webhook source, revoking its shared secret immediately.
func (c Webhooks) Remove(g *gin.Context) {
	ctx := g.Request.Context()
	d := webhooksData{
		Configured:  c.API.Configured(),
		DomainKnown: c.Domain != "",
	}
	source, err := url.PathUnescape(g.Param("source"))
	if err != nil {
		source = g.Param("source")
	}

	switch {
	case !d.Configured:
		d.ErrKey = "err.api_not_configured"
	case source == "":
		d.ErrKey = "webhooks.err_remove"
		d.ErrRaw = "empty source in request path"
	default:
		if err := c.API.DeleteWebhook(ctx, source); err != nil {
			d.ErrKey, d.ErrRaw = "webhooks.err_remove", err.Error()
		} else {
			d.MsgKey, d.MsgArg = "webhooks.msg_removed", source
		}
	}

	c.load(g, &d)
	c.Views.Fragment(g, "webhooks", d)
}

// load lists the sources, recording an error if the list itself fails. A
// removed source is also the case where the operator most needs the list, so
// this runs on every mutation as well as on the read path.
func (c Webhooks) load(g *gin.Context, d *webhooksData) {
	base := c.endpointBase()
	d.Base = base
	srcs, err := c.API.ListWebhooks(g.Request.Context())
	if err != nil {
		if d.ErrKey == "" {
			d.ErrKey, d.ErrRaw = "webhooks.err_list", err.Error()
		}
		return
	}

	d.Known = true
	d.Count = len(srcs)
	d.Sources = make([]webhookRow, 0, len(srcs))
	for _, s := range srcs {
		d.Sources = append(d.Sources, webhookRow{
			Source:      s.Source,
			Description: s.Description,
			CreatedAt:   s.CreatedAt,
			LastUsedAt:  s.LastUsedAt,
			Endpoint:    endpointFor(base, s.Source),
		})
	}
}

// webhookDeliveryLimit is the history window the deliveries panel reads. The
// daemon clamps it to its own page size, so this is a request, not a promise.
const webhookDeliveryLimit = 50

// deliveryRow is one recorded delivery attempt, rendered in a source's history
// table. Status is the daemon's own word: an unknown status is shown as-is
// rather than mapped onto a state this console does not know.
type deliveryRow struct {
	ID        int64
	Status    string
	StackName string
	Revision  int64
	RepoURL   string
	File      string
	Error     string
	CreatedAt int64

	// StateKey/Pill name the recorded status in the reader's language; a status
	// the console does not know is printed as the daemon wrote it.
	StateKey string
	Pill     string
}

func deliveryRows(ds []pmapi.WebhookDelivery) []deliveryRow {
	out := make([]deliveryRow, 0, len(ds))
	for _, d := range ds {
		key, pill := deliveryStatusKey(d.Status)
		out = append(out, deliveryRow{
			ID: d.ID, Status: d.Status, StackName: d.StackName,
			Revision: d.Revision, RepoURL: d.RepoURL, File: d.File,
			Error: d.Error, CreatedAt: d.CreatedAt,
			StateKey: key, Pill: pill,
		})
	}
	return out
}

// deliveryStatusKey maps a recorded status onto a state word and a pill. The
// receiver's vocabulary is small but not fixed, so an unrecognised status is
// shown raw rather than dropped.
func deliveryStatusKey(status string) (key, pill string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "accepted", "ok", "success", "succeeded", "delivered":
		return "webhookdeliveries.status_accepted", "good"
	case "unauthorized", "rejected", "bad_signature", "invalid_signature":
		return "webhookdeliveries.status_unauthorized", "warn"
	case "error", "failed", "failure":
		return "webhookdeliveries.status_error", "bad"
	}
	return "", "plain"
}

// Deliveries renders the recorded delivery history for one source. A failed read
// leaves DeliveriesKnown false, which the fragment renders as unknown — never as
// "no deliveries", which would claim the receiver has been silent.
func (c Webhooks) Deliveries(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := webhooksData{Source: g.Param("source"), Configured: configured, DeliveryLimit: webhookDeliveryLimit}
	if !configured {
		d.ErrKey = "err.api_not_configured"
		c.Views.Fragment(g, "webhookdeliveries", d)
		return
	}
	ds, err := c.API.ListWebhookDeliveries(ctx, d.Source, webhookDeliveryLimit)
	if err != nil {
		d.ErrKey, d.ErrRaw = "err.webhook_deliveries", err.Error()
		c.Views.Fragment(g, "webhookdeliveries", d)
		return
	}
	d.Deliveries, d.DeliveriesKnown = deliveryRows(ds), true
	c.Views.Fragment(g, "webhookdeliveries", d)
}
