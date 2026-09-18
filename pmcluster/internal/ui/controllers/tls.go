package controllers

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/pmapi"
)

// daysNote renders a human-readable days-left hint for the console.
func daysNote(days int) string {
	if days < 0 {
		return "expired " + strconv.Itoa(-days) + " days ago"
	}
	return strconv.Itoa(days) + " days left"
}

// TLS lists the cluster's own (default) certificate and the per-host
// certificates, and lets the operator replace/add/remove them.
//
// Both flows are identical apart from the target: the main cert covers the
// cluster's own domain (e.g. nextrum-sy.com) and is wired into the default
// Traefik certificate; a per-host cert covers a customer's own domain. Certs +
// keys arrive as TEXT (pasted into textareas) — never a file upload — and are
// sent to the pmcluster daemon API, which validates them, materializes versioned
// Swarm secrets and refreshes Traefik.
type TLS struct{ *Controller }

type tlsData struct {
	Site  *tlsSiteRow
	Hosts []tlsRow
	Error string
	Msg   string
}

type tlsSiteFormData struct {
	Action string
	Cert   string
	Key    string
	Error  string
}

type tlsHostFormData struct {
	Action string
	Host   string
	Cert   string
	Key    string
	Error  string
}

type tlsSiteRow struct {
	Domain    string
	Expires   string
	NotBefore string
	SANs      string
	CertHash  string
	Updated   string
	DaysLeft  int
	Note      string
	Expiring  bool
	Expired   bool
}

type tlsRow struct {
	Host     string
	Expires  string
	SANs     string
	DaysLeft int
	Note     string
	Expiring bool
	Expired  bool
}

// expiryWarnDays is how close to expiry (in days) a certificate must be before
// the console flags it for renewal.
const expiryWarnDays = 30

// expiry computes the days left until an RFC3339 timestamp and the warn/expired
// flags. Unparseable timestamps yield zeros (no warning).
func expiry(rfc3339 string) (days int, expiring, expired bool) {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return 0, false, false
	}
	d := int(time.Until(t).Hours() / 24)
	return d, d <= expiryWarnDays, d < 0
}

func joinSANs(sans []string) string {
	out := ""
	for i, s := range sans {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func siteCertRow(sc *pmapi.SiteCert) *tlsSiteRow {
	if sc == nil || sc.Domain == "" {
		return nil
	}
	days, expiring, expired := expiry(sc.NotAfter)
	return &tlsSiteRow{
		Domain:    sc.Domain,
		Expires:   sc.NotAfter,
		NotBefore: sc.NotBefore,
		SANs:      joinSANs(sc.SANs),
		CertHash:  sc.CertHash,
		Updated:   sc.UpdatedAt,
		DaysLeft:  days,
		Note:      daysNote(days),
		Expiring:  expiring,
		Expired:   expired,
	}
}

func tlsRows(hosts []pmapi.HostCert) []tlsRow {
	out := make([]tlsRow, 0, len(hosts))
	for _, h := range hosts {
		days, expiring, expired := expiry(h.NotAfter)
		out = append(out, tlsRow{
			Host: h.Host, Expires: h.NotAfter, SANs: joinSANs(h.SANs),
			DaysLeft: days, Note: daysNote(days), Expiring: expiring, Expired: expired,
		})
	}
	return out
}

// fetch loads the site cert + per-host certs into d, recording the first error.
func (c TLS) fetch(g *gin.Context, d *tlsData) {
	ctx := g.Request.Context()
	if sc, err := c.API.GetSiteCert(ctx); err != nil {
		if d.Error == "" {
			d.Error = err.Error()
		}
	} else {
		d.Site = siteCertRow(sc)
	}
	hosts, err := c.API.ListHostCerts(ctx)
	if err != nil {
		if d.Error == "" {
			d.Error = err.Error()
		}
		return
	}
	d.Hosts = tlsRows(hosts)
}

// Page renders the TLS certificates page.
func (c TLS) Page(g *gin.Context) {
	_, _, configured := c.loadParams(g.Request.Context())
	d := tlsData{}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "tls", d)
		return
	}
	c.fetch(g, &d)
	c.Views.Fragment(g, "tls", d)
}

// SiteNew renders the modal form for uploading/renewing the main certificate.
func (c TLS) SiteNew(g *gin.Context) {
	d := tlsSiteFormData{Action: WebBase + "/tls/site"}
	if _, _, configured := c.loadParams(g.Request.Context()); !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
	}
	c.Views.Fragment(g, "tlssiteform", d)
}

// HostNew renders the modal form for adding a per-host certificate.
func (c TLS) HostNew(g *gin.Context) {
	d := tlsHostFormData{Action: WebBase + "/tls"}
	if _, _, configured := c.loadParams(g.Request.Context()); !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
	}
	c.Views.Fragment(g, "tlshostform", d)
}

func (c TLS) tlsSiteFormError(g *gin.Context, d tlsSiteFormData) {
	g.Header("HX-Retarget", "#modal-body")
	g.Status(http.StatusUnprocessableEntity)
	c.Views.Fragment(g, "tlssiteform", d)
}

func (c TLS) tlsHostFormError(g *gin.Context, d tlsHostFormData) {
	g.Header("HX-Retarget", "#modal-body")
	g.Status(http.StatusUnprocessableEntity)
	c.Views.Fragment(g, "tlshostform", d)
}

// SetSite replaces the cluster's own (default) certificate. On validation or
// API failure the modal form is re-rendered (HX-Retarget) so the operator can
// correct the input; on success the page fragment replaces #view and the
// modal closes.
func (c TLS) SetSite(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	cert := g.PostForm("cert")
	key := g.PostForm("key")
	d := tlsSiteFormData{Action: WebBase + "/tls/site", Cert: cert, Key: key}

	switch {
	case !configured:
		d.Error = "pmcluster API not configured. Open Settings first."
	case cert == "" || key == "":
		d.Error = "Certificate and private key are both required."
	default:
		sc, err := c.API.UpdateSiteCert(ctx, cert, key)
		if err != nil {
			d.Error = err.Error()
		} else {
			page := tlsData{Msg: "Site certificate for " + sc.Domain + " updated and Traefik refreshed."}
			c.fetch(g, &page)
			c.Views.Fragment(g, "tls", page)
			return
		}
	}
	c.tlsSiteFormError(g, d)
}

// Add stores a per-host certificate submitted as text. Same modal-error flow
// as SetSite.
func (c TLS) Add(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	host := g.PostForm("host")
	cert := g.PostForm("cert")
	key := g.PostForm("key")
	d := tlsHostFormData{Action: WebBase + "/tls", Host: host, Cert: cert, Key: key}

	switch {
	case !configured:
		d.Error = "pmcluster API not configured. Open Settings first."
	case host == "" || cert == "" || key == "":
		d.Error = "Host, certificate and key are all required."
	default:
		if _, err := c.API.AddHostCert(ctx, host, cert, key); err != nil {
			d.Error = err.Error()
		} else {
			page := tlsData{Msg: "Certificate for " + host + " stored and Traefik refreshed."}
			c.fetch(g, &page)
			c.Views.Fragment(g, "tls", page)
			return
		}
	}
	c.tlsHostFormError(g, d)
}

// Remove deletes a per-host certificate.
func (c TLS) Remove(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := tlsData{}
	host, err := url.PathUnescape(g.Param("host"))
	if err != nil {
		host = g.Param("host")
	}

	if configured && host != "" {
		if err := c.API.RemoveHostCert(ctx, host); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = "Removed certificate for " + host + "."
		}
	} else if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
	}

	c.fetch(g, &d)
	c.Views.Fragment(g, "tls", d)
}
