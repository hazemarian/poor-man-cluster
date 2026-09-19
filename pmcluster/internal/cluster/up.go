package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/workflow"
)

type UpInput struct {
	Domain string
	// Either CertPath+KeyPath OR ACMEEmail must be set; mutually exclusive.
	// CertPath/KeyPath: operator-provided PEM files loaded into Swarm secrets.
	// ACMEEmail: Traefik issues + renews via Let's Encrypt HTTP-01.
	// On a re-run these may be omitted to reuse the stored TLS state.
	CertPath              string
	KeyPath               string
	ACMEEmail             string
	ForceTLSMode          bool
	TraefikAdminUser      string
	OpenObserveAdminEmail string
	ConfigDir             string
	Version               string
}

// UpResult includes plaintext passwords for any credentials newly minted
// on this run; existing ones are returned with NewlyCreated=false.
type UpResult struct {
	NewNetworks          []string
	NewSecrets           []string
	NewConfigs           []string
	StacksDeployed       []string
	BootstrapCredentials map[string]*ManagedCredential
}

type UpDeps struct {
	Store    *store.Store
	Cipher   *credentials.Cipher
	Docker   runtime.Client
	Deployer StackDeployer
	Stdout   io.Writer
}

// Up brings the cluster up end-to-end as a sequence of named steps: preflight
// → overlay networks → TLS secrets → bootstrap credentials → rendered configs
// → stack deploys (infra → edge → observability → backup) → health wait →
// install state persistence.
//
// OpenObserve runs with the admin email:password (ZO_ROOT_USER_EMAIL /
// ZO_ROOT_USER_PASSWORD from the openobserve_admin credential + the
// zo_root_user_password Swarm secret). The OTel collector and the Traefik
// openobserve-auto-auth middleware use the SAME admin credentials — no
// provisioning API calls, no automation user, no ingestion token.
func Up(ctx context.Context, deps UpDeps, in UpInput) (*UpResult, error) {

	state, err := loadTLSSettings(ctx, deps.Store)
	if err != nil {
		return nil, err
	}
	merged, err := mergeTLSState(in, state)
	if err != nil {
		return nil, err
	}
	in = merged

	if err := validateUpInput(in); err != nil {
		return nil, err
	}

	// cluster up is init-only: it must never touch a cluster that is already
	// running. A persisted domain or TLS state means `cluster update` owns the
	// cluster from here on.
	if deps.Store != nil {
		installed, err := clusterInstalled(ctx, deps.Store)
		if err != nil {
			return nil, err
		}
		if installed {
			return nil, fmt.Errorf("cluster already initialised — run `pmcluster cluster update` instead (cluster up is for fresh installs only)")
		}
	}

	out := io.Discard
	if deps.Stdout != nil {
		out = deps.Stdout
	}

	res := &UpResult{}
	var (
		certSecret, keySecret string
		creds                 map[string]*ManagedCredential
		render                RenderInput
		otelConfigName        string
		otelConfigCreated     bool
		sso                   ssoState
		ssoSecret             string
	)

	wf := workflow.NewWorkflow(out)
	wf.Add("Preflight: Docker reachable, Swarm active, this node is a manager", func(ctx context.Context) error {
		return Preflight(ctx, deps.Docker)
	})
	wf.Add("Syncing platform config templates into the store", func(ctx context.Context) error {
		syncRes, err := SyncPlatformConfigs(ctx, deps.Store, in.Version)
		if err != nil {
			return err
		}
		for _, n := range syncRes.Created {
			fmt.Fprintf(out, "  ✓ %s recorded in store\n", n)
		}
		for _, n := range syncRes.Updated {
			fmt.Fprintf(out, "  ✓ %s updated in store\n", n)
		}
		return nil
	})
	wf.Add("Ensuring overlay networks", func(ctx context.Context) error {
		created, err := EnsureBundledNetworks(ctx, deps.Docker)
		if err != nil {
			return fmt.Errorf("ensure networks: %w", err)
		}
		res.NewNetworks = created
		return nil
	})
	wf.Add("TLS certificate (Let's Encrypt or operator cert/key)", func(ctx context.Context) error {
		if in.ACMEEmail != "" {
			fmt.Fprintf(out, "  ▶ TLS via Let's Encrypt (Traefik HTTP-01) — port 80 must be reachable from the internet\n")
			return nil
		}
		var certCreated, keyCreated bool
		certSecret, certCreated, err = EnsureVersionedSecretFromFile(ctx, deps.Docker, deps.Store, "cert", in.CertPath)
		if err != nil {
			return fmt.Errorf("ensure cert secret: %w", err)
		}
		keySecret, keyCreated, err = EnsureVersionedSecretFromFile(ctx, deps.Docker, deps.Store, "key", in.KeyPath)
		if err != nil {
			return fmt.Errorf("ensure key secret: %w", err)
		}
		if certCreated || keyCreated {
			res.NewSecrets = append(res.NewSecrets, certSecret, keySecret)
		}
		return nil
	})
	wf.Add("Bootstrapping managed credentials (Traefik / OpenObserve / edge)", func(ctx context.Context) error {
		credMgr := &CredentialsManager{
			Store:  deps.Store,
			Cipher: deps.Cipher,
			Docker: deps.Docker,
		}
		creds, err = credMgr.Bootstrap(ctx, BootstrapInput{
			TraefikAdminUser:      in.TraefikAdminUser,
			OpenObserveAdminEmail: in.OpenObserveAdminEmail,
		})
		if err != nil {
			return fmt.Errorf("bootstrap credentials: %w", err)
		}
		res.BootstrapCredentials = creds
		for name, c := range creds {
			switch {
			case c.NewlyCreated && c.SwarmSecretCreated:
				res.NewSecrets = append(res.NewSecrets, c.SwarmSecretName)
				fmt.Fprintf(out, "  ✓ %-20s newly created (swarm secret %s)\n", name, c.SwarmSecretName)
			case c.NewlyCreated && !c.SwarmSecretCreated:
				fmt.Fprintf(out, "  ⚠ %-20s store updated, but Swarm secret %s pre-existed (passwords may diverge)\n", name, c.SwarmSecretName)
			case !c.NewlyCreated && c.SwarmSecretCreated:
				res.NewSecrets = append(res.NewSecrets, c.SwarmSecretName)
				fmt.Fprintf(out, "  ✓ %-20s already in store; Swarm secret %s recreated\n", name, c.SwarmSecretName)
			default:
				if c.UsernameChanged {
					fmt.Fprintf(out, "  ✓ %-20s username updated to %s\n", name, c.Username)
				} else {
					fmt.Fprintf(out, "  ✓ %-20s already present\n", name)
				}
			}
		}
		if creds["edge_admin"] != nil {
			fmt.Fprintf(out, "  ✔ edge console credentials minted — retrieve the admin password with `pmcluster credentials show edge_admin`\n")
		}
		if obsCred := creds["openobserve_admin"]; obsCred != nil && obsCred.UsernameChanged {
			fmt.Fprintf(out, "  ⚠ OpenObserve email changed → resetting data volume so new credentials take effect\n")
			if err := deps.Docker.VolumeRemove(ctx, "observability_openobserve_data"); err != nil {
				return fmt.Errorf("reset openobserve data volume: %w (manual: docker volume rm observability_openobserve_data)", err)
			}
		}
		return nil
	})
	wf.Add("Rendering and creating Docker configs (OTel pipeline, Traefik dynamic)", func(ctx context.Context) error {
		openobsCred := creds["openobserve_admin"]
		if openobsCred == nil {
			return fmt.Errorf("internal: openobserve_admin credential missing after bootstrap")
		}

		sso, err = loadSSOSettings(ctx, deps.Store)
		if err != nil {
			return err
		}
		if err := sso.validate(); err != nil {
			return err
		}
		ssoSecret = ""
		if sso.Enabled {
			// sso_cookie_secret is minted by the credentials bootstrap; pull
			// its plaintext so oauth2-proxy gets a stable cookie secret.
			cookieCred, ok := creds["sso_cookie_secret"]
			if !ok || cookieCred == nil {
				return fmt.Errorf("internal: sso_cookie_secret credential missing after bootstrap (SSO enabled)")
			}
			ssoSecret = cookieCred.Password
		}

		hostCerts, err := loadHostCertEntries(ctx, deps.Store, in.Domain)
		if err != nil {
			return err
		}

		render = RenderInput{
			Domain:                   in.Domain,
			OpenObserveAdminEmail:    openobsCred.Username,
			OpenObserveAdminPassword: openobsCred.Password,
			OpenObserveBasicAuth:     openObserveBasicAuth(openobsCred.Username, openobsCred.Password),
			ACMEEmail:                in.ACMEEmail,
			ConfigDir:                in.ConfigDir,
			ConfigStore:              deps.Store,
			DataDir:                  filepath.Dir(in.ConfigDir),
			HostCerts:                hostCerts,
			CertSecretName:           certSecret,
			KeySecretName:            keySecret,
			EdgeImage:                EdgeImageFor(),
			EdgeLoginDisabled:        loadEdgeLoginDisabled(ctx, deps.Store),
			SSOEnabled:               sso.Enabled,
			SSOCookieSecret:          ssoSecret,
			SSOClientID:              sso.ClientID,
			SSOClientSecret:          sso.ClientSecret,
			SSOGitHubOrg:             sso.GitHubOrg,
			SSOCookieExpire:          sso.CookieExpire,
		}

		otelConfigName, otelConfigCreated, err = ensureOTelConfig(ctx, deps, in.Version, render)
		if err != nil {
			return err
		}
		if otelConfigCreated {
			res.NewConfigs = append(res.NewConfigs, otelConfigName)
		}
		render.OTelConfigName = otelConfigName

		traefikYAML, err := RenderTraefikDynamic(render)
		if err != nil {
			return err
		}
		traefikConfigName, traefikConfigCreated, err := EnsureConfig(ctx, deps.Docker, "pmcluster_traefik_dynamic", traefikYAML, in.Version)
		if err != nil {
			return err
		}
		if traefikConfigCreated {
			res.NewConfigs = append(res.NewConfigs, traefikConfigName)
		}
		render.TraefikConfigName = traefikConfigName

		edgeName, edgeCreated, err := ensureEdgeConfig(ctx, deps.Docker, in.Version, render)
		if err != nil {
			return err
		}
		if edgeCreated {
			res.NewConfigs = append(res.NewConfigs, edgeName)
		}
		return nil
	})
	wf.Add("Deploying stacks (infra → edge → observability → backup)", func(ctx context.Context) error {
		stacks := []stackName{StackInfra, StackEdge, StackObservability, StackBackup}
		if sso.Enabled {
			// sso (oauth2-proxy) is deployed alongside the platform stacks so
			// Traefik's forwardAuth middleware has a live backend.
			stacks = append(stacks, StackSSO)
		}
		for _, s := range stacks {
			if err := deployStack(ctx, out, deps.Deployer, s, render); err != nil {
				return err
			}
			res.StacksDeployed = append(res.StacksDeployed, string(s))
		}
		return nil
	})
	wf.Add("Waiting for all services to become healthy", func(ctx context.Context) error {
		if err := WaitHealthyStacks(ctx, deps.Docker, out); err != nil {
			return fmt.Errorf("health check: %w", err)
		}
		return nil
	})
	wf.Add("Snapshotting rendered configs into the store", func(ctx context.Context) error {
		if deps.Store == nil {
			return nil
		}
		rendered, err := renderPlatformConfigs(render)
		if err != nil {
			return fmt.Errorf("render platform configs for persistence: %w", err)
		}
		for name, content := range rendered {
			if err := deps.Store.SetRendered(ctx, name, string(content)); err != nil && !errors.Is(err, store.ErrConfigNotFound) {
				return fmt.Errorf("store rendered config %s: %w", name, err)
			}
		}
		return nil
	})
	wf.Add("Persisting install state", func(ctx context.Context) error {
		return persistInstallState(ctx, deps, in)
	})
	wf.Add("Cluster up complete.", func(ctx context.Context) error {
		return nil
	})

	if err := wf.Run(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// persistInstallState records the install inputs used on this (possibly
// idempotent) bring-up: TLS mode + cert/key paths, domain, and OO admin email.
func persistInstallState(ctx context.Context, deps UpDeps, in UpInput) error {
	if deps.Store == nil {
		return nil
	}
	state := tlsState{
		Mode:      requestedTLSMode(in),
		CertPath:  in.CertPath,
		KeyPath:   in.KeyPath,
		ACMEEmail: in.ACMEEmail,
	}
	if err := state.save(ctx, deps.Store); err != nil {
		return fmt.Errorf("persist tls state: %w", err)
	}
	for k, v := range map[string]string{
		settingDomain:  in.Domain,
		settingOOEmail: in.OpenObserveAdminEmail,
	} {
		if err := deps.Store.SetSetting(ctx, k, v); err != nil {
			return fmt.Errorf("persist %s: %w", k, err)
		}
	}
	return nil
}

// ensureOTelConfig renders the collector config and ensures its versioned
// Docker config exists, returning the (versioned) name and whether it was newly
// created. Used by up/update so the collector's ingestion auth is content-aware.
func ensureOTelConfig(ctx context.Context, deps UpDeps, version string, render RenderInput) (string, bool, error) {
	otelYAML, err := RenderOTelCollectorConfig(render)
	if err != nil {
		return "", false, err
	}
	return EnsureConfig(ctx, deps.Docker, "pmcluster_otel_config", otelYAML, version)
}

func validateUpInput(in UpInput) error {
	if in.Domain == "" {
		return fmt.Errorf("--domain is required")
	}
	if in.OpenObserveAdminEmail == "" {
		return fmt.Errorf("--openobserve-email is required (used as OpenObserve admin login)")
	}
	hasBYOTLS := in.CertPath != "" || in.KeyPath != ""
	hasACME := in.ACMEEmail != ""
	if hasBYOTLS && hasACME {
		return fmt.Errorf("choose ONE TLS mode: --acme-email (Let's Encrypt) OR --cert + --key (operator-supplied)")
	}
	if !hasBYOTLS && !hasACME {
		return fmt.Errorf("a TLS mode is required: --acme-email <you@host> (Let's Encrypt) OR --cert <pem> --key <pem>")
	}
	if hasBYOTLS && (in.CertPath == "" || in.KeyPath == "") {
		return fmt.Errorf("--cert and --key must both be provided")
	}
	return nil
}
