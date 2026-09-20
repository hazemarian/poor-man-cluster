package controllers

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// ---------------------------------------------------------------------------
// TLS page plumbing.
//
// The v2 page's whole point is that CHAIN status and EXPIRES are two separate
// facts, and that neither of them may be invented: the daemon API exposes the
// leaf's NotBefore/NotAfter/SANs but says nothing about whether the served
// chain carries its intermediates. So expiry is always rendered from the
// timestamp (or as "not reported" when the timestamp is missing/unparseable)
// and the chain column is rendered as unreported; the two are never folded
// into one "certificate OK" badge.
//
// Three states per read:
//   - known and present  -> the row renders
//   - known and empty    -> an empty state that names the upload action
//   - read failed        -> Known=false, a banner with the raw payload; never
//     rendered as "no certificate"
// ---------------------------------------------------------------------------

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
	Site *tlsCertRow
	// SiteKnown separates "the site certificate was read and none is recorded"
	// from "the read failed": the second one is never rendered as "no cert".
	SiteKnown bool

	Hosts      []tlsCertRow
	HostsKnown bool

	Count     int64
	Expiring  int64
	Expired   int64
	Uncovered int64

	WarnDays     int64
	SiteExpired  bool
	SiteExpiring bool

	ErrKey string
	ErrRaw string
	MsgKey string
	MsgA   string
}

// StatsKnown is true only when BOTH certificate reads succeeded, so the
// counters are real totals rather than a partial sum presented as one.
func (d tlsData) StatsKnown() bool { return d.SiteKnown && d.HostsKnown }

// tlsCertRow is one certificate — the site's own cert and a per-host cert carry
// the same facts, so they share one row shape.
type tlsCertRow struct {
	Domain string
	// ExpiresKnown is false when the API did not return a parsable NotAfter:
	// the row then says so instead of showing "0 days left".
	ExpiresKnown bool
	NotAfter     string
	NotBefore    string
	DaysLeft     int64 // signed: negative once expired
	AbsDays      int64 // absolute day count, for the "expired N days ago" pill
	Expiring     bool
	Expired      bool

	SANs     []string
	SANCount int64
	SANList  string
	// CoversHost is true when the certificate's own SAN list contains the host
	// it is filed under (exact or wildcard match). An empty SAN list leaves it
	// false and the template renders "not reported" rather than "not covered".
	CoversHost bool

	CertSecret string
	KeySecret  string
	CertHash   string
	KeyHash    string
	Created    string
	Updated    string
}

type tlsSiteFormData struct {
	Action string
	Cert   string
	Key    string
	ErrKey string
	ErrRaw string
}

type tlsHostFormData struct {
	Action string
	Host   string
	Cert   string
	Key    string
	ErrKey string
	ErrRaw string
}

// expiryWarnDays is how close to expiry (in days) a certificate must be before
// the console flags it for renewal.
const expiryWarnDays = 30

// expiry parses an RFC3339 timestamp into a day count. known is false when the
// timestamp is empty or unparseable — the caller must then render "not
// reported" instead of a zero.
func expiry(rfc3339 string) (days int64, expiring, expired, known bool) {
	if strings.TrimSpace(rfc3339) == "" {
		return 0, false, false, false
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return 0, false, false, false
	}
	d := int64(time.Until(t).Hours() / 24)
	return d, d <= expiryWarnDays, d < 0, true
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func joinSANs(sans []string) string { return strings.Join(sans, ", ") }

// sansCover reports whether a SAN list covers host (exact match, or a
// single-label wildcard such as *.example.com).
func sansCover(sans []string, host string) bool {
	if host == "" {
		return false
	}
	host = strings.ToLower(host)
	for _, s := range sans {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == host {
			return true
		}
		if strings.HasPrefix(s, "*.") {
			suffix := s[1:] // ".example.com"
			if strings.HasSuffix(host, suffix) && strings.Count(host, ".") == strings.Count(suffix, ".") {
				return true
			}
		}
	}
	return false
}

func siteCertRow(sc *pmapi.SiteCert) *tlsCertRow {
	if sc == nil || sc.Domain == "" {
		return nil
	}
	days, expiring, expired, known := expiry(sc.NotAfter)
	return &tlsCertRow{
		Domain:       sc.Domain,
		ExpiresKnown: known,
		NotAfter:     sc.NotAfter,
		NotBefore:    sc.NotBefore,
		DaysLeft:     days,
		AbsDays:      abs(days),
		Expiring:     expiring,
		Expired:      expired,
		SANs:         sc.SANs,
		SANCount:     int64(len(sc.SANs)),
		SANList:      joinSANs(sc.SANs),
		CertSecret:   sc.CertSecret,
		KeySecret:    sc.KeySecret,
		CertHash:     sc.CertHash,
		KeyHash:      sc.KeyHash,
		Created:      sc.CreatedAt,
		Updated:      sc.UpdatedAt,
	}
}

func tlsRows(hosts []pmapi.HostCert) []tlsCertRow {
	out := make([]tlsCertRow, 0, len(hosts))
	for _, h := range hosts {
		days, expiring, expired, known := expiry(h.NotAfter)
		out = append(out, tlsCertRow{
			Domain:       h.Host,
			ExpiresKnown: known,
			NotAfter:     h.NotAfter,
			NotBefore:    h.NotBefore,
			DaysLeft:     days,
			AbsDays:      abs(days),
			Expiring:     expiring,
			Expired:      expired,
			SANs:         h.SANs,
			SANCount:     int64(len(h.SANs)),
			SANList:      joinSANs(h.SANs),
			CoversHost:   sansCover(h.SANs, h.Host),
			CertSecret:   h.CertSecret,
			KeySecret:    h.KeySecret,
			CertHash:     h.CertHash,
			KeyHash:      h.KeyHash,
			Created:      h.CreatedAt,
			Updated:      h.UpdatedAt,
		})
	}
	return out
}

// isNotFound reports whether err is the API's 404 — "nothing recorded", which
// is an empty state, not a failure.
func isNotFound(err error) bool {
	var pe *pmapi.Error
	return errors.As(err, &pe) && pe.Status == http.StatusNotFound
}

// summarise fills the counts the stat row and the banners read. It only counts
// what was actually read: a failed side leaves its counters at zero AND the
// template refuses to print them (SiteKnown/HostsKnown gate the stats).
func summarise(d *tlsData) {
	hosts := int64(len(d.Hosts))
	d.Count = hosts
	if d.Site != nil {
		d.Count++
	}
	d.Expiring = 0
	d.Expired = 0
	d.Uncovered = 0
	if d.Site != nil {
		if d.Site.Expired {
			d.Expired++
		} else if d.Site.Expiring {
			d.Expiring++
		}
		d.SiteExpired = d.Site.Expired
		d.SiteExpiring = d.Site.Expired || d.Site.Expiring
	}
	for _, h := range d.Hosts {
		if h.Expired {
			d.Expired++
		} else if h.Expiring {
			d.Expiring++
		}
		if h.SANCount == 0 || !h.CoversHost {
			d.Uncovered++
		}
	}
}

// fetch loads the site cert + per-host certs into d. Each read carries its own
// Known flag; the first error becomes the page banner.
func (c TLS) fetch(g *gin.Context, d *tlsData) {
	ctx := g.Request.Context()

	switch sc, err := c.API.GetSiteCert(ctx); {
	case err == nil:
		d.SiteKnown = true
		d.Site = siteCertRow(sc)
	case isNotFound(err):
		d.SiteKnown = true
		d.Site = nil
	default:
		if d.ErrKey == "" {
			d.ErrKey, d.ErrRaw = humanErr("err.tls_site", err)
		}
	}

	switch hosts, err := c.API.ListHostCerts(ctx); {
	case err == nil:
		d.HostsKnown = true
		d.Hosts = tlsRows(hosts)
	case isNotFound(err):
		d.HostsKnown = true
	default:
		if d.ErrKey == "" {
			d.ErrKey, d.ErrRaw = humanErr("err.tls_hosts", err)
		}
	}

	if d.SiteKnown && d.HostsKnown {
		summarise(d)
	}
}

// Page renders the TLS certificates page.
func (c TLS) Page(g *gin.Context) {
	_, _, configured := c.loadParams(g.Request.Context())
	d := tlsData{WarnDays: expiryWarnDays}
	if !configured {
		d.ErrKey = errKeyNotConfigured
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
		d.ErrKey = errKeyNotConfigured
	}
	c.Views.Fragment(g, "tlssiteform", d)
}

// HostNew renders the modal form for adding a per-host certificate.
func (c TLS) HostNew(g *gin.Context) {
	d := tlsHostFormData{Action: WebBase + "/tls"}
	if _, _, configured := c.loadParams(g.Request.Context()); !configured {
		d.ErrKey = errKeyNotConfigured
	}
	c.Views.Fragment(g, "tlshostform", d)
}

// siteFormError re-renders the modal with the operator's input kept, so a
// rejected PEM never costs them the paste.
func (c TLS) siteFormError(g *gin.Context, d tlsSiteFormData) {
	g.Header("HX-Retarget", "#modal-body")
	g.Status(http.StatusUnprocessableEntity)
	c.Views.Fragment(g, "tlssiteform", d)
}

func (c TLS) hostFormError(g *gin.Context, d tlsHostFormData) {
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
		d.ErrKey = errKeyNotConfigured
	case strings.TrimSpace(cert) == "" || strings.TrimSpace(key) == "":
		// Local validation, not an upstream failure: no raw payload to show.
		d.ErrKey = "tls.err_required"
	default:
		sc, err := c.API.UpdateSiteCert(ctx, cert, key)
		if err != nil {
			d.ErrKey, d.ErrRaw = humanErr("err.tls_site", err)
		} else {
			page := tlsData{WarnDays: expiryWarnDays}
			domain := ""
			if sc != nil {
				domain = sc.Domain
			}
			// The dictionary owns these keys: `tls.msg_site_set` never existed, so the
			// message rendered as a raw key after a successful upload.
			page.MsgKey, page.MsgA = "tls.msg_site_updated", domain
			c.fetch(g, &page)
			c.Views.Fragment(g, "tls", page)
			return
		}
	}
	c.siteFormError(g, d)
}

// Add stores a per-host certificate submitted as text. Same modal-error flow
// as SetSite.
func (c TLS) Add(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	host := strings.TrimSpace(g.PostForm("host"))
	cert := g.PostForm("cert")
	key := g.PostForm("key")
	d := tlsHostFormData{Action: WebBase + "/tls", Host: host, Cert: cert, Key: key}

	switch {
	case !configured:
		d.ErrKey = errKeyNotConfigured
	case host == "":
		d.ErrKey = "tls.err_host_required"
	case strings.TrimSpace(cert) == "" || strings.TrimSpace(key) == "":
		d.ErrKey = "tls.err_required"
	default:
		if _, err := c.API.AddHostCert(ctx, host, cert, key); err != nil {
			d.ErrKey, d.ErrRaw = humanErr("err.tls_hosts", err)
		} else {
			page := tlsData{WarnDays: expiryWarnDays}
			page.MsgKey, page.MsgA = "tls.msg_host_stored", host
			c.fetch(g, &page)
			c.Views.Fragment(g, "tls", page)
			return
		}
	}
	c.hostFormError(g, d)
}

// Remove deletes a per-host certificate.
func (c TLS) Remove(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := tlsData{WarnDays: expiryWarnDays}
	host, err := url.PathUnescape(g.Param("host"))
	if err != nil {
		host = g.Param("host")
	}

	switch {
	case !configured:
		d.ErrKey = errKeyNotConfigured
	case host == "":
		d.ErrKey = "tls.err_host_required"
	default:
		if err := c.API.RemoveHostCert(ctx, host); err != nil {
			d.ErrKey, d.ErrRaw = humanErr("err.tls_remove", err)
		} else {
			d.MsgKey, d.MsgA = "tls.msg_host_removed", host
		}
	}

	c.fetch(g, &d)
	c.Views.Fragment(g, "tls", d)
}
