// Package controllers implements the MVC "C" for the pmcluster UI.
//
// Each controller is a set of gin handlers that pull the relevant model
// (pmapi calls / local store reads) and hand render-ready data to the views.
// They stay thin: no query/JSON logic of their own beyond HTTP form plumbing.
package controllers

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/pmapi"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/store"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/views"
)

// Setting keys persisted in the local store.
const (
	keyAPIURL = "pmcluster_api_url"
	keyToken  = "pmcluster_api_token"
)

// Controller carries the shared dependencies every handler needs. References,
// not ownership: it is constructed once in the ui package and passed around.
type Controller struct {
	Store    *store.Store
	API      *pmapi.Client
	Views    *views.Renderer
	Auth     *middleware.Auth
	Version  string
	EnvAPI   string // API base URL from env (fallback when no override set)
	EnvToken string // API token from env (fallback)
}

// loadParams reads the stored overrides and the env fallbacks into the running
// client, returning the effective base/token and whether it is configured.
func (c *Controller) loadParams(ctx context.Context) (apiURL, token string, configured bool) {
	apiURL = c.EnvAPI
	token = c.EnvToken

	cfg, err := c.Store.GetSettings(ctx)
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
