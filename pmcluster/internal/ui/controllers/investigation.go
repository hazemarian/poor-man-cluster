package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// Investigations serves bounded operator jobs and admin configuration.
type Investigations struct{ *Controller }
type investigationData struct {
	Job           investigation.Job
	Policy        investigation.Policy
	Budget        investigation.Budget
	Error         bool
	KeyConfigured bool
	Saved         bool
}

func (c Investigations) originAllowed(g *gin.Context) bool {
	expected := c.PublicOrigin
	if expected == "" {
		scheme := "http"
		if g.Request.TLS != nil {
			scheme = "https"
		}
		expected = scheme + "://" + g.Request.Host
	}
	want, err := url.Parse(expected)
	if err != nil {
		return false
	}
	raw := g.GetHeader("Origin")
	if raw == "" {
		raw = g.GetHeader("Referer")
	}
	got, err := url.Parse(raw)
	if err != nil || got.User != nil || got.Host == "" || !strings.EqualFold(got.Scheme, want.Scheme) || !strings.EqualFold(got.Host, want.Host) || !strings.EqualFold(g.Request.Host, want.Host) {
		return false
	}
	return true
}

func (c Investigations) ledger(g *gin.Context) (pmapi.InvestigationLedger, bool) {
	if c.ObservabilityRate != nil && !c.ObservabilityRate.Allow() {
		return pmapi.InvestigationLedger{}, false
	}
	base, token, ok := c.loadParams(g.Request.Context())
	if !ok {
		return pmapi.InvestigationLedger{}, false
	}
	api := pmapi.New(base, token, 5*time.Second)
	_, err := api.Me(g.Request.Context())
	return pmapi.InvestigationLedger{Client: api}, err == nil
}

// Start gathers authorized evidence, then queues one explicitly requested model call.
func (c Investigations) Start(g *gin.Context) {
	if !c.originAllowed(g) {
		g.Status(http.StatusForbidden)
		return
	}
	g.Request.Body = http.MaxBytesReader(g.Writer, g.Request.Body, 8192)
	if g.Request.ParseForm() != nil {
		g.Status(http.StatusBadRequest)
		return
	}
	question := g.PostForm("question")
	if len(question) == 0 || len(question) > 2000 {
		g.Status(http.StatusBadRequest)
		return
	}
	if c.Investigation == nil || !c.Investigation.Configured() {
		c.unavailable(g)
		return
	}
	g.Request.URL.RawQuery = url.Values{"service": {g.PostForm("service")}, "range": {g.PostForm("range")}}.Encode()
	d := (Observability{c.Controller}).load(g, true)
	if !d.ScopeKnown || !d.Evidence.Known || d.ErrKey != "" {
		c.unavailable(g)
		return
	}
	ledger := pmapi.InvestigationLedger{Client: d.api}
	ctx, cancel := context.WithTimeout(g.Request.Context(), 5*time.Second)
	defer cancel()
	p, err := ledger.Policy(ctx)
	if err != nil || !p.Enabled || !p.Sharing {
		c.unavailable(g)
		return
	}
	rows := make([]observability.Row, len(d.Evidence.Rows))
	for i, row := range d.Evidence.Rows {
		rows[i] = observability.RowForLog(row.Service, row.Severity, row.Body, row.Timestamp)
	}
	in := investigation.Compact(d.redactor.Text(question), d.Evidence.WindowStart, d.Evidence.WindowEnd, rows, d.Evidence.Truncated)
	job, err := c.Investigation.Start(ctx, ledger, c.owner(g, d.api), p, in)
	if err != nil {
		c.unavailable(g)
		return
	}
	c.Views.Fragment(g, "investigationjob", investigationData{Job: job})
}

// Show polls a submitter-owned job without triggering provider requests.
func (c Investigations) Show(g *gin.Context) {
	ledger, ok := c.ledger(g)
	if !ok {
		g.Status(http.StatusForbidden)
		return
	}
	if c.Investigation == nil {
		g.Status(http.StatusNotFound)
		return
	}
	job, ok := c.Investigation.Get(g.Param("id"), c.owner(g, ledger.Client))
	if !ok {
		g.Status(http.StatusNotFound)
		return
	}
	c.Views.Fragment(g, "investigationjob", investigationData{Job: job})
}

// Cancel requests cancellation without refunding uncertain dispatched spend.
func (c Investigations) Cancel(g *gin.Context) {
	if !c.originAllowed(g) {
		g.Status(http.StatusForbidden)
		return
	}
	ledger, ok := c.ledger(g)
	if !ok {
		g.Status(http.StatusForbidden)
		return
	}
	if c.Investigation == nil || !c.Investigation.Cancel(g.Param("id"), c.owner(g, ledger.Client)) {
		g.Status(http.StatusNotFound)
		return
	}
	c.Show(g)
}

// Settings renders the administrator's nonsecret provider policy.
func (c Investigations) Settings(g *gin.Context) {
	ledger, ok := c.ledger(g)
	if !ok {
		g.Status(http.StatusForbidden)
		return
	}
	p, err := ledger.Policy(g.Request.Context())
	b, berr := ledger.Budget(g.Request.Context())
	c.Views.Fragment(g, "investigationsettings", investigationData{Policy: p, Budget: b, Error: err != nil || berr != nil, KeyConfigured: c.Investigation != nil && c.Investigation.Configured()})
}

// SaveSettings persists only validated nonsecret values through the admin daemon API.
func (c Investigations) SaveSettings(g *gin.Context) {
	if !c.originAllowed(g) {
		g.Status(http.StatusForbidden)
		return
	}
	ledger, ok := c.ledger(g)
	if !ok {
		g.Status(http.StatusForbidden)
		return
	}
	g.Request.Body = http.MaxBytesReader(g.Writer, g.Request.Body, 8192)
	if g.Request.ParseForm() != nil {
		g.Status(400)
		return
	}
	input, e1 := strconv.ParseInt(g.PostForm("input_tokens"), 10, 64)
	output, e2 := strconv.ParseInt(g.PostForm("output_tokens"), 10, 64)
	daily, e3 := strconv.ParseInt(g.PostForm("daily_tokens"), 10, 64)
	p := investigation.Policy{Enabled: g.PostForm("enabled") == "on", Sharing: g.PostForm("sharing") == "on", BaseURL: g.PostForm("base_url"), Model: g.PostForm("model"), DailyTokens: daily, MaxInputTokens: input, MaxOutputTokens: output, TokenField: g.PostForm("token_field")}
	if e1 != nil || e2 != nil || e3 != nil || p.Validate() != nil {
		g.Status(http.StatusBadRequest)
		return
	}
	if ledger.SetPolicy(g.Request.Context(), p) != nil {
		g.Status(http.StatusBadGateway)
		return
	}
	c.Settings(g)
}

func (c Investigations) owner(g *gin.Context, api *pmapi.Client) string {
	sum := sha256.Sum256([]byte(api.BaseURL() + "\x00" + api.Token()))
	return middleware.CurrentUser(g).Username + ":" + hex.EncodeToString(sum[:])
}

func (c Investigations) unavailable(g *gin.Context) {
	c.Views.Fragment(g, "investigationerror", struct{}{})
}
