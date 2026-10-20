// Package controllers implements the MVC "C" for the pmcluster UI.
//
// Each controller is a set of gin handlers that pull the relevant model
// (pmapi calls / local store reads) and hand render-ready data to the views.
// They stay thin: no query/JSON logic of their own beyond HTTP form plumbing.
package controllers

import (
	"context"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
	"golang.org/x/time/rate"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/views"
)

// Setting keys persisted in the local store. Defined in the store package so
// the ui bootstrap seeds the same keys the controllers read.
const (
	keyAPIURL = store.KeyAPIURL
	keyToken  = store.KeyToken
)

// WebBase is the URL prefix the operator console is mounted under
// (pmcluster.<domain>/web/*). Native signed edge sessions gate the prefix by
// default; explicit Basic Auth and SSO are selected by the edge deployment.
// The edge's /api and /webhook routes are outside it. Every template path and
// redirect the controllers emit must start here.
const WebBase = "/web"

// Controller carries the shared dependencies every handler needs. References,
// not ownership: it is constructed once in the ui package and passed around.
type Controller struct {
	Store    *store.Store
	API      *pmapi.Client
	Views    *views.Renderer
	Auth     *middleware.Auth
	Version  string
	Domain   string
	EnvAPI   string
	EnvToken string
	// Telemetry is nil when Parseable is not configured or unavailable. The
	// console still renders daemon-derived health in that state.
	ObservabilityRate *rate.Limiter
	Investigation     *investigation.Manager
	PublicOrigin      string
	Telemetry         observability.Source
	Redactor          observability.Redactor
}

// loadParams reads the stored overrides and the env fallbacks into the running
// client, returning the effective base/token and whether it is configured.
func (c *Controller) loadParams(ctx context.Context) (apiURL, token string, configured bool) {
	apiURL = c.EnvAPI
	token = c.EnvToken

	cfg, err := c.Store.Settings(ctx)
	if err == nil {
		if v, ok := cfg[keyAPIURL]; ok && v != "" {
			apiURL = v
		}
		if v, ok := cfg[keyToken]; ok && v != "" {
			token = v
		}
	}
	c.API.SetBase(apiURL)
	c.API.SetToken(token)
	return apiURL, token, apiURL != "" && token != ""
}
