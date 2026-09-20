package controllers

import (
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
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
