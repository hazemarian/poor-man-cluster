package controllers

import (
	"net/url"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/pmapi"
)

// TLS lists per-host certificates and lets the operator add/remove them.
// Certs + keys arrive as TEXT (pasted into textareas) — never a file upload —
// and are sent to the pmcluster daemon API, which writes them to disk and
// refreshes Traefik.
type TLS struct{ *Controller }

type tlsData struct {
	Hosts []tlsRow
	Error string
	Msg   string
}

type tlsRow struct {
	Host    string
	Expires string
	SANs    string
}

func tlsRows(hosts []pmapi.HostCert) []tlsRow {
	out := make([]tlsRow, 0, len(hosts))
	for _, h := range hosts {
		sans := ""
		for i, s := range h.SANs {
			if i > 0 {
				sans += ", "
			}
			sans += s
		}
		out = append(out, tlsRow{Host: h.Host, Expires: h.NotAfter, SANs: sans})
	}
	return out
}

// Page renders the TLS certificates page.
func (c TLS) Page(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := tlsData{}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "tls", d)
		return
	}
	hosts, err := c.API.ListHostCerts(ctx)
	if err != nil {
		d.Error = err.Error()
		c.Views.Fragment(g, "tls", d)
		return
	}
	d.Hosts = tlsRows(hosts)
	c.Views.Fragment(g, "tls", d)
}

// Add stores a per-host certificate submitted as text.
func (c TLS) Add(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := tlsData{}
	host := g.PostForm("host")
	cert := g.PostForm("cert")
	key := g.PostForm("key")

	if configured && host != "" && cert != "" && key != "" {
		if _, err := c.API.AddHostCert(ctx, host, cert, key); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = "Certificate for " + host + " stored and Traefik refreshed."
		}
	} else if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
	} else {
		d.Error = "Host, certificate and key are all required."
	}

	hosts, err := c.API.ListHostCerts(ctx)
	if err != nil && d.Error == "" {
		d.Error = err.Error()
	}
	d.Hosts = tlsRows(hosts)
	c.Views.Fragment(g, "tls", d)
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

	hosts, err := c.API.ListHostCerts(ctx)
	if err != nil && d.Error == "" {
		d.Error = err.Error()
	}
	d.Hosts = tlsRows(hosts)
	c.Views.Fragment(g, "tls", d)
}
