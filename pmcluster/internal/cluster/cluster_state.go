package cluster

import (
	"context"
	"fmt"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// cluster_settings keys persisted on a successful `up` and read back by both
// `up` (idempotent re-runs) and `update` (knows what to re-provision without
// re-bootstrapping).
const (
	settingTLSMode          = "tls_mode"
	settingTLSCertPath      = "tls_cert_path"
	settingTLSKeyPath       = "tls_key_path"
	settingTLSACME          = "tls_acme_email"
	settingDomain           = "domain"
	settingOOEmail          = "oo_admin_email"
	settingTraefikAdminUser = "traefik_admin_user"

	// SSO settings. SSO is a feature flag (settingSSOEnabled): when enabled the
	// Traefik admin-auth basicAuth middleware is swapped for a forwardAuth
	// middleware pointing at the oauth2-proxy sidecar (sso stack), so identity
	// comes from the OAuth provider (GitHub) instead of the htpasswd file.
	settingSSOEnabled      = "sso_enabled"
	settingSSOProvider     = "sso_provider"
	settingSSOClientID     = "sso_client_id"
	settingSSOClientSecret = "sso_client_secret"
	settingSSOGitHubOrg    = "sso_github_org"
	settingSSOCookieExpire = "sso_cookie_expire"

	// settingEdgeLoginDisabled controls the edge console's own session login.
	// Default "true" in swarm deployments: Traefik admin-auth (or SSO) already
	// gates /web, so the console login + Users CRUD are hidden. Local standalone
	// runs leave it unset (or "false") to keep password login.
	settingEdgeLoginDisabled = "edge_login_disabled"
)

// Setting* accessors expose the persisted settings keys for CLI surfaces
// (e.g. the interactive `pmcluster setup` wizard) that read/write them
// directly instead of through the cluster workflow.
func SettingDomain() string            { return settingDomain }
func SettingTLSMode() string           { return settingTLSMode }
func SettingTLSCertPath() string       { return settingTLSCertPath }
func SettingTLSKeyPath() string        { return settingTLSKeyPath }
func SettingTLSACME() string           { return settingTLSACME }
func SettingOOEmail() string           { return settingOOEmail }
func SettingTraefikAdminUser() string  { return settingTraefikAdminUser }
func SettingSSOEnabled() string        { return settingSSOEnabled }
func SettingSSOProvider() string       { return settingSSOProvider }
func SettingSSOClientID() string       { return settingSSOClientID }
func SettingSSOClientSecret() string   { return settingSSOClientSecret }
func SettingSSOGitHubOrg() string      { return settingSSOGitHubOrg }
func SettingSSOCookieExpire() string   { return settingSSOCookieExpire }
func SettingEdgeLoginDisabled() string { return settingEdgeLoginDisabled }

// ClusterInstalled reports whether this store already holds a live cluster.
func ClusterInstalled(ctx context.Context, st *store.Store) bool {
	ok, err := clusterInstalled(ctx, st)
	return err == nil && ok
}

// clusterInstalled reports whether this store already holds a live cluster.
// `cluster up` is init-only, so it refuses to run when either the persisted
// domain/TLS install state or the cluster-scope platform config rows exist.
func clusterInstalled(ctx context.Context, st *store.Store) (bool, error) {
	if st == nil {
		return false, nil
	}
	if st.GetSettingDefault(ctx, settingDomain, "") != "" {
		return true, nil
	}
	if st.GetSettingDefault(ctx, settingTLSMode, "") != "" {
		return true, nil
	}
	rows, err := st.ListConfigs(ctx, "cluster", "")
	if err != nil {
		return false, fmt.Errorf("list cluster configs: %w", err)
	}
	return len(rows) > 0, nil
}

// tlsState is the persisted TLS install state.
type tlsState struct {
	Mode      string
	CertPath  string
	KeyPath   string
	ACMEEmail string
}

// loadTLSSettings reads the persisted TLS state (empty when never installed).
func loadTLSSettings(ctx context.Context, st *store.Store) (tlsState, error) {
	if st == nil {
		return tlsState{}, nil
	}
	return tlsState{
		Mode:      st.GetSettingDefault(ctx, settingTLSMode, ""),
		CertPath:  st.GetSettingDefault(ctx, settingTLSCertPath, ""),
		KeyPath:   st.GetSettingDefault(ctx, settingTLSKeyPath, ""),
		ACMEEmail: st.GetSettingDefault(ctx, settingTLSACME, ""),
	}, nil
}

func (t tlsState) save(ctx context.Context, st *store.Store) error {
	if st == nil {
		return nil
	}
	vals := map[string]string{
		settingTLSMode:     t.Mode,
		settingTLSCertPath: t.CertPath,
		settingTLSKeyPath:  t.KeyPath,
		settingTLSACME:     t.ACMEEmail,
	}
	for k, v := range vals {
		if err := st.SetSetting(ctx, k, v); err != nil {
			return fmt.Errorf("persist %s: %w", k, err)
		}
	}
	return nil
}

// TLSState is the exported view of the persisted TLS install state, used by
// CLI surfaces (the `pmcluster setup` wizard) to write the TLS mode before
// handing off to cluster up/update.
type TLSState struct {
	Mode      string
	CertPath  string
	KeyPath   string
	ACMEEmail string
}

// Save persists the TLS install state into the store settings.
func (t TLSState) Save(ctx context.Context, st *store.Store) error {
	return tlsState(t).save(ctx, st)
}

// ssoState is the persisted SSO install state. SSO is opt-in via the
// sso_enabled flag; the provider is always "github" for now (the only
// provider oauth2-proxy is configured with).
type ssoState struct {
	Enabled      bool
	Provider     string
	ClientID     string
	ClientSecret string
	GitHubOrg    string
	// CookieExpire is the oauth2-proxy session cookie lifetime (e.g. "1h",
	// "24h", "168h"). Default "1h" so a member removed from the GitHub org
	// loses access within the hour instead of keeping a 7-day session.
	CookieExpire string
}

// defaultSSOCookieExpire is the tightened default session lifetime used when
// sso_cookie_expire is unset (oauth2-proxy's own default is 168h).
const defaultSSOCookieExpire = "1h"

// loadSSOSettings reads the persisted SSO state (all empty when never set).
func loadSSOSettings(ctx context.Context, st *store.Store) (ssoState, error) {
	if st == nil {
		return ssoState{}, nil
	}
	return ssoState{
		Enabled:      st.GetSettingDefault(ctx, settingSSOEnabled, "") == "true",
		Provider:     st.GetSettingDefault(ctx, settingSSOProvider, ""),
		ClientID:     st.GetSettingDefault(ctx, settingSSOClientID, ""),
		ClientSecret: st.GetSettingDefault(ctx, settingSSOClientSecret, ""),
		GitHubOrg:    st.GetSettingDefault(ctx, settingSSOGitHubOrg, ""),
		CookieExpire: st.GetSettingDefault(ctx, settingSSOCookieExpire, defaultSSOCookieExpire),
	}, nil
}

func (s ssoState) validate() error {
	if !s.Enabled {
		return nil
	}
	if s.Provider == "" {
		return fmt.Errorf("SSO is enabled but no provider is configured (only \"github\" is supported)")
	}
	if s.Provider != "github" {
		return fmt.Errorf("SSO provider %q is not supported (only \"github\" is supported)", s.Provider)
	}
	if s.ClientID == "" || s.ClientSecret == "" {
		return fmt.Errorf("SSO provider %q requires client ID + client secret", s.Provider)
	}
	if _, err := time.ParseDuration(s.CookieExpire); err != nil {
		return fmt.Errorf("SSO cookie expire %q is not a valid duration (e.g. 1h, 24h, 168h): %w", s.CookieExpire, err)
	}
	return nil
}

// loadEdgeLoginDisabled reports whether the edge console's own session login
// is disabled (default true — the swarm deployment always gates /web behind
// Traefik admin-auth or SSO, so the console login is redundant; local standalone
// runs set edge_login_disabled=false to keep password login).
func loadEdgeLoginDisabled(ctx context.Context, st *store.Store) bool {
	if st == nil {
		return true
	}
	return st.GetSettingDefault(ctx, settingEdgeLoginDisabled, "true") == "true"
}

// requestedTLSMode derives the TLS mode the operator asked for on this run,
// or "" if no TLS flags were supplied (meaning "use the stored mode").
func requestedTLSMode(in UpInput) string {
	if in.ACMEEmail != "" {
		return "acme"
	}
	if in.CertPath != "" || in.KeyPath != "" {
		return "cert"
	}
	return ""
}

// mergeTLSState reconciles the operator's requested input with the stored
// install state so an idempotent re-run doesn't require re-passing the TLS
// flags and can't silently flip the TLS mode.
//
//   - No stored state → input is used as-is.
//   - Stored state + no TLS flags on this run → fill in the stored cert/key
//     paths (cert mode) or ACME email (acme mode).
//   - Stored mode != requested mode → error unless ForceTLSMode is set.
//   - Stored mode == requested mode → any flags supplied win, others are
//     filled from storage.
func mergeTLSState(in UpInput, state tlsState) (UpInput, error) {
	if state.Mode == "" {
		return in, nil
	}
	req := requestedTLSMode(in)
	if req != "" && req != state.Mode && !in.ForceTLSMode {
		return in, fmt.Errorf("cluster is installed with TLS mode %q but you requested %q; pass --force-tls-mode to switch (an accidental mode flip can permanently break the edge)", state.Mode, req)
	}
	switch state.Mode {
	case "cert":
		if in.CertPath == "" {
			in.CertPath = state.CertPath
		}
		if in.KeyPath == "" {
			in.KeyPath = state.KeyPath
		}
	case "acme":
		if in.ACMEEmail == "" {
			in.ACMEEmail = state.ACMEEmail
		}
	}
	return in, nil
}
