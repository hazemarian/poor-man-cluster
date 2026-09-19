// Package ui wires the gin-based pmcluster operator UI.
//
// Architecture is MVC: gin handlers (controllers) render HTMX template
// fragments (views) and call the pmcluster daemon through a typed client
// (pmapi) backed by the local store (users + settings persist in SQLite).
// The UI talks to the pmcluster API over Bearer auth, so it runs equally well
// inside the swarm (pinned to host.docker.internal:9090) or as a standalone
// image on the internet.
package ui

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config holds every tunable for the UI service, sourced from the environment.
type Config struct {
	ListenAddr      string
	DataDir         string
	PMAPIURL        string
	PMAPIToken      string
	EnvUser         string
	EnvPass         string
	SessionSecret   []byte
	CookieName      string
	UpstreamTimeout time.Duration
	AppVersion      string
	// ClusterDomain (PMCLUSTER_DOMAIN) is the public cluster domain (e.g.
	// example.com). The console renders external links to OpenObserve
	// (observ.<domain>) and the Traefik dashboard (traefik.<domain>) from it.
	// Set by the edge stack template ([[.Domain]]); local standalone runs leave
	// it empty and the links are hidden.
	ClusterDomain string
	// LoginDisabled (EDGE_LOGIN_DISABLED) turns off the console's own session
	// login. This is the deployed-edge mode: Traefik's admin-auth gate protects
	// /web/*, so the console skips its own login, the user CRUD page and the
	// first-run setup page are hidden, and Require() lets every request through
	// as a synthetic admin. Local/standalone deployments leave it unset so
	// login + setup + user CRUD work normally.
	LoginDisabled bool
}

const (
	defaultDataDir    = "./data"
	defaultCookieName = "pmui_session"
	defaultTimeout    = 15 * time.Second
)

// FromEnv builds a Config from the process environment with sane defaults.
func FromEnv() Config {
	return Config{
		ListenAddr:      envOr("LISTEN_ADDR", ":8080"),
		DataDir:         envOr("DATA_DIR", defaultDataDir),
		PMAPIURL:        envOr("PMCLUSTER_API_URL", ""),
		PMAPIToken:      envOr("PMCLUSTER_API_TOKEN", ""),
		EnvUser:         envOr("PMCLUSTER_UI_USER", ""),
		EnvPass:         envOr("PMCLUSTER_UI_PASS", ""),
		SessionSecret:   []byte(envOr("PMCLUSTER_UI_SECRET", "pmcluster-ui-insecure-default-change-me")),
		CookieName:      envOr("SESSION_COOKIE", defaultCookieName),
		UpstreamTimeout: envDur("UPSTREAM_TIMEOUT", defaultTimeout),
		AppVersion:      envOr("APP_VERSION", "dev"),
		ClusterDomain:   envOr("PMCLUSTER_DOMAIN", ""),
		LoginDisabled:   envBool("EDGE_LOGIN_DISABLED"),
	}
}

func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Validate checks required config before serving. The session secret is always
// present via a default; cmd/edge warns about the weak default.
func (c Config) Validate() error {
	if c.DataDir == "" {
		return fmt.Errorf("data_dir is required")
	}
	if len(c.EnvUser) > 0 && len(c.EnvPass) == 0 {
		return fmt.Errorf("PMCLUSTER_UI_USER set without PMCLUSTER_UI_PASS")
	}
	if len(c.SessionSecret) < 16 {
		return fmt.Errorf("PMCLUSTER_UI_SECRET must be at least 16 bytes")
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
