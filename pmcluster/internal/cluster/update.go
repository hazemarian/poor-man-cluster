package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/workflow"
)

// UpdateInput carries the config-dir + build version for `cluster update`.
// Unlike Up, UpdateInput carries no TLS/domain/credential fields — those are
// read back from the persisted install state so update needs no re-bootstrap.
type UpdateInput struct {
	ConfigDir string
	Version   string
}

// UpdateResult reports which configs/secrets were rotated and which stacks
// were re-deployed on this update. A no-op update (nothing changed) returns
// empty/zero results and deploys nothing.
type UpdateResult struct {
	OTelConfig     string
	OTelCreated    bool
	TraefikConfig  string
	TraefikCreated bool
	CertSecret     string
	KeySecret      string
	CertCreated    bool
	KeyCreated     bool
	EdgeConfig     string
	EdgeCreated    bool
	EdgeDeployed   bool
	StacksDeployed []string
}

type UpdateDeps struct {
	Store    *store.Store
	Cipher   *credentials.Cipher
	Docker   runtime.Client
	Deployer StackDeployer
	Stdout   io.Writer
}

// Update re-provisions the OTel collector + Traefik dynamic configs and the
// TLS cert/key, pushing only what changed into Docker, and re-deploying only
// the stack(s) whose rendered content moved.
//
// It first syncs the platform config templates into the store to the current
// build version — the DB is the single source of truth, so stale rows are
// refreshed from the embedded defaults and console edits at the current
// version are preserved. It never re-bootstraps credentials and never resets
// volumes. It is content-aware end to end: each stack's freshly rendered
// compose is hashed and compared against the stored rendered_hash, so an
// unchanged stack is not redeployed (a no-op update is just a report).
func Update(ctx context.Context, deps UpdateDeps, in UpdateInput) (*UpdateResult, error) {
	out := io.Discard
	if deps.Stdout != nil {
		out = deps.Stdout
	}
	res := &UpdateResult{}

	var (
		state     tlsState
		domain    string
		render    RenderInput
		sso       ssoState
		ssoSecret string
	)

	wf := workflow.NewWorkflow(out)
	wf.Add("Preflight: Docker reachable, Swarm active", func(ctx context.Context) error {
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

	wf.Add("Loading persisted install state (domain, TLS, credentials)", func(ctx context.Context) error {
		if deps.Store == nil {
			return fmt.Errorf("update requires a store (config_dir must be initialised via `pmcluster init`)")
		}
		var err error
		state, err = loadTLSSettings(ctx, deps.Store)
		if err != nil {
			return err
		}
		domain = deps.Store.GetSettingDefault(ctx, settingDomain, "")
		if domain == "" {
			return fmt.Errorf("no persisted domain found — run `cluster up` before `cluster update`")
		}
		return nil
	})

	wf.Add("Loading SSO settings + cookie secret", func(ctx context.Context) error {
		var err error
		sso, err = loadSSOSettings(ctx, deps.Store)
		if err != nil {
			return err
		}
		if err := sso.validate(); err != nil {
			return err
		}
		ssoSecret = ""
		if sso.Enabled {
			cookieCred, err := deps.Store.GetCredential(ctx, "sso_cookie_secret")
			switch {
			case errors.Is(err, store.ErrCredentialNotFound):
				// SSO enabled on a cluster whose bootstrap predates the
				// credential — self-heal by minting it now (idempotent).
				mgr := &CredentialsManager{Store: deps.Store, Cipher: deps.Cipher, Docker: deps.Docker, Deployer: deps.Deployer}
				mc, cerr := mgr.Ensure(ctx, "sso_cookie_secret")
				if cerr != nil {
					return fmt.Errorf("bootstrap sso_cookie_secret (SSO enabled): %w", cerr)
				}
				ssoSecret = mc.Password
			case err != nil:
				return fmt.Errorf("load sso_cookie_secret credential (SSO enabled): %w", err)
			default:
				plain, derr := deps.Cipher.Decrypt(cookieCred.PasswordCiphertext)
				if derr != nil {
					return fmt.Errorf("decrypt sso_cookie_secret: %w", derr)
				}
				ssoSecret = string(plain)
			}
		}
		return nil
	})

	wf.Add("Loading OpenObserve credentials", func(ctx context.Context) error {
		ooCred, err := deps.Store.GetCredential(ctx, "openobserve_admin")
		if err != nil {
			return fmt.Errorf("load openobserve_admin credential (run `cluster up` first): %w", err)
		}
		ooPass, err := deps.Cipher.Decrypt(ooCred.PasswordCiphertext)
		if err != nil {
			return fmt.Errorf("decrypt openobserve_admin password: %w", err)
		}
		render = RenderInput{
			Domain:                   domain,
			OpenObserveAdminEmail:    ooCred.Username,
			OpenObserveAdminPassword: string(ooPass),
			OpenObserveBasicAuth:     openObserveBasicAuth(ooCred.Username, string(ooPass)),
			ACMEEmail:                state.ACMEEmail,
			ConfigDir:                in.ConfigDir,
			ConfigStore:              deps.Store,
			DataDir:                  filepath.Dir(in.ConfigDir),
			EdgeImage:                EdgeImageFor(),
			EdgeLoginDisabled:        loadEdgeLoginDisabled(ctx, deps.Store),
			BackupAllNodes:           loadBackupAllNodes(ctx, deps.Store),
			SSOEnabled:               sso.Enabled,
			SSOCookieSecret:          ssoSecret,
			SSOClientID:              sso.ClientID,
			SSOClientSecret:          sso.ClientSecret,
			SSOGitHubOrg:             sso.GitHubOrg,
			SSOCookieExpire:          sso.CookieExpire,
		}
		hostCerts, err := loadHostCertEntries(ctx, deps.Store, domain)
		if err != nil {
			return err
		}
		render.HostCerts = hostCerts
		return nil
	})

	wf.Add("TLS certificate (ACME or stored cert/key)", func(ctx context.Context) error {
		if render.ACMEEmail != "" {
			return nil
		}
		if state.CertPath == "" || state.KeyPath == "" {
			return fmt.Errorf("TLS is not ACME (no ACME email) but no cert/key paths are persisted — run `cluster up` with --cert/--key (or --acme-email) before `cluster update`")
		}
		certName, certCreated, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, deps.Store, "cert", state.CertPath)
		if err != nil {
			return fmt.Errorf("ensure cert secret: %w", err)
		}
		keyName, keyCreated, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, deps.Store, "key", state.KeyPath)
		if err != nil {
			return fmt.Errorf("ensure key secret: %w", err)
		}
		res.CertSecret, res.KeySecret = certName, keyName
		res.CertCreated, res.KeyCreated = certCreated, keyCreated
		render.CertSecretName, render.KeySecretName = certName, keyName
		warnSiteCertExpiry(ctx, deps.Store, domain, deps.Stdout)
		return nil
	})

	wf.Add("Rendering OTel + Traefik + edge configs (content-aware)", func(ctx context.Context) error {
		otelYAML, err := RenderOTelCollectorConfig(render)
		if err != nil {
			return err
		}
		otelName, otelCreated, err := EnsureConfig(ctx, deps.Docker, "pmcluster_otel_config", otelYAML, in.Version)
		if err != nil {
			return err
		}
		res.OTelConfig, res.OTelCreated = otelName, otelCreated
		render.OTelConfigName = otelName

		traefikYAML, err := RenderTraefikDynamic(render)
		if err != nil {
			return err
		}
		traefikName, traefikCreated, err := EnsureConfig(ctx, deps.Docker, "pmcluster_traefik_dynamic", traefikYAML, in.Version)
		if err != nil {
			return err
		}
		res.TraefikConfig, res.TraefikCreated = traefikName, traefikCreated
		render.TraefikConfigName = traefikName

		edgeName, edgeCreated, err := ensureEdgeConfig(ctx, deps.Docker, in.Version, render)
		if err != nil {
			return err
		}
		res.EdgeConfig, res.EdgeCreated = edgeName, edgeCreated
		return nil
	})

	wf.Add("Reconciling platform stacks (rendered content vs stored hash)", func(ctx context.Context) error {
		// Render all six platform configs with the fully-populated render
		// (config names + cert secrets substituted) so the snapshots are
		// valid YAML and comparable across runs.
		rendered, err := renderPlatformConfigs(render)
		if err != nil {
			return err
		}

		// Per-stack re-deploy decision: hash the freshly rendered compose and
		// compare it with the stored rendered_hash of the matching config row.
		// Any change in the stack template, the versioned Docker config name it
		// mounts, the edge image tag, or the TLS secret names it references
		// shows up here — the DB hash is the single source of truth.
		var redeploy []stackName
		order := []stackName{StackObservability, StackInfra, StackEdge, StackBackup}
		if sso.Enabled {
			order = append(order, StackSSO)
		}
		for _, s := range order {
			cfgName := string(s) + "-stack"
			fresh := rendered[cfgName]
			row, err := deps.Store.GetConfig(ctx, cfgName)
			if err != nil && !errors.Is(err, store.ErrConfigNotFound) {
				return fmt.Errorf("read rendered hash for %s: %w", cfgName, err)
			}
			if row != nil && row.RenderedHash == store.ConfigHash(string(fresh)) {
				continue
			}
			redeploy = append(redeploy, s)
		}

		// Snapshot every rendered config into the store (rendered_content +
		// rendered_hash) so the console reads them back and the next update
		// has a baseline to compare against.
		for name, content := range rendered {
			if err := deps.Store.SetRendered(ctx, name, string(content)); err != nil && !errors.Is(err, store.ErrConfigNotFound) {
				return fmt.Errorf("store rendered config %s: %w", name, err)
			}
		}

		for _, s := range redeploy {
			fmt.Fprintf(out, "  ▶ %s content changed → re-deploying\n", string(s))
			if err := deployStack(ctx, out, deps.Deployer, s, render); err != nil {
				return err
			}
			res.StacksDeployed = append(res.StacksDeployed, string(s))
			if s == StackEdge {
				res.EdgeDeployed = true
			}
		}
		if len(redeploy) == 0 {
			fmt.Fprintf(out, "  ▶ No rendered content changed — nothing to redeploy.\n")
		}
		return nil
	})

	wf.Add("Cluster update complete.", func(ctx context.Context) error { return nil })

	if err := wf.Run(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// renderPlatformConfigs renders all six platform configs (four stack composes
// plus otel-collector-config and traefik-dynamic) with a fully populated
// render — the versioned Docker config names, TLS secret names and edge image
// tag are substituted, so every snapshot is valid YAML and stable across runs.
func renderPlatformConfigs(render RenderInput) (map[string][]byte, error) {
	out := make(map[string][]byte, 6)
	stacks := []stackName{StackInfra, StackObservability, StackBackup, StackEdge}
	if render.SSOEnabled {
		stacks = append(stacks, StackSSO)
	}
	for _, s := range stacks {
		y, err := LoadComposeFile(s, render)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", s, err)
		}
		out[string(s)+"-stack"] = y
	}
	otelYAML, err := RenderOTelCollectorConfig(render)
	if err != nil {
		return nil, fmt.Errorf("render otel-collector-config: %w", err)
	}
	out["otel-collector-config"] = otelYAML
	traefikYAML, err := RenderTraefikDynamic(render)
	if err != nil {
		return nil, fmt.Errorf("render traefik-dynamic: %w", err)
	}
	out["traefik-dynamic"] = traefikYAML
	return out, nil
}

// deployStack renders and deploys a single stack, streaming progress.
func deployStack(ctx context.Context, out io.Writer, d StackDeployer, s stackName, render RenderInput) error {
	composeYAML, err := LoadComposeFile(s, render)
	if err != nil {
		return fmt.Errorf("load compose for %s: %w", s, err)
	}
	fmt.Fprintf(out, "  ▶ docker stack deploy %s\n", s)
	if err := d.DeployStack(ctx, string(s), composeYAML); err != nil {
		return fmt.Errorf("deploy %s: %w", s, err)
	}
	return nil
}

// RenderClusterConfigs renders the current platform configs (post-substitution
// YAML) without deploying any stack. It mirrors Update's render construction —
// including the versioned Docker config names for OTel/Traefik/edge — so the
// output matches what a `cluster update` would deploy and is valid YAML.
// Keep in sync with Update.
func RenderClusterConfigs(ctx context.Context, deps UpdateDeps, in UpdateInput) (map[string]string, error) {
	if err := Preflight(ctx, deps.Docker); err != nil {
		return nil, err
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("rendering configs requires a store (config_dir must be initialised via `pmcluster init`)")
	}
	state, err := loadTLSSettings(ctx, deps.Store)
	if err != nil {
		return nil, err
	}
	domain := deps.Store.GetSettingDefault(ctx, settingDomain, "")
	if domain == "" {
		return nil, fmt.Errorf("no persisted domain found — run `cluster up` before requesting rendered configs")
	}
	ooCred, err := deps.Store.GetCredential(ctx, "openobserve_admin")
	if err != nil {
		return nil, fmt.Errorf("load openobserve_admin credential (run `cluster up` first): %w", err)
	}
	ooPass, err := deps.Cipher.Decrypt(ooCred.PasswordCiphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt openobserve_admin password: %w", err)
	}
	render := RenderInput{
		Domain:                   domain,
		OpenObserveAdminEmail:    ooCred.Username,
		OpenObserveAdminPassword: string(ooPass),
		OpenObserveBasicAuth:     openObserveBasicAuth(ooCred.Username, string(ooPass)),
		ACMEEmail:                state.ACMEEmail,
		ConfigDir:                in.ConfigDir,
		ConfigStore:              deps.Store,
		DataDir:                  filepath.Dir(in.ConfigDir),
		EdgeImage:                EdgeImageFor(),
		EdgeLoginDisabled:        loadEdgeLoginDisabled(ctx, deps.Store),
		BackupAllNodes:           loadBackupAllNodes(ctx, deps.Store),
	}
	sso, err := loadSSOSettings(ctx, deps.Store)
	if err != nil {
		return nil, err
	}
	render.SSOEnabled = sso.Enabled
	render.SSOClientID = sso.ClientID
	render.SSOClientSecret = sso.ClientSecret
	render.SSOGitHubOrg = sso.GitHubOrg
	render.SSOCookieExpire = sso.CookieExpire
	if sso.Enabled {
		cookieCred, err := deps.Store.GetCredential(ctx, "sso_cookie_secret")
		switch {
		case errors.Is(err, store.ErrCredentialNotFound):
			mgr := &CredentialsManager{Store: deps.Store, Cipher: deps.Cipher, Docker: deps.Docker, Deployer: deps.Deployer}
			mc, cerr := mgr.Ensure(ctx, "sso_cookie_secret")
			if cerr != nil {
				return nil, fmt.Errorf("bootstrap sso_cookie_secret (SSO enabled): %w", cerr)
			}
			render.SSOCookieSecret = mc.Password
		case err != nil:
			return nil, fmt.Errorf("load sso_cookie_secret credential (SSO enabled): %w", err)
		default:
			plain, derr := deps.Cipher.Decrypt(cookieCred.PasswordCiphertext)
			if derr != nil {
				return nil, fmt.Errorf("decrypt sso_cookie_secret: %w", derr)
			}
			render.SSOCookieSecret = string(plain)
		}
	}
	hostCerts, err := loadHostCertEntries(ctx, deps.Store, domain)
	if err != nil {
		return nil, err
	}
	render.HostCerts = hostCerts
	if render.ACMEEmail == "" {
		if state.CertPath == "" || state.KeyPath == "" {
			return nil, fmt.Errorf("TLS is not ACME (no ACME email) but no cert/key paths are persisted — run `cluster up` with --cert/--key (or --acme-email)")
		}
		certName, _, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, deps.Store, "cert", state.CertPath)
		if err != nil {
			return nil, err
		}
		keyName, _, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, deps.Store, "key", state.KeyPath)
		if err != nil {
			return nil, err
		}
		render.CertSecretName, render.KeySecretName = certName, keyName
	}

	// Materialise the versioned Docker config names so the rendered stacks
	// reference real configs (valid YAML) and the snapshots are hashable.
	otelYAML, err := RenderOTelCollectorConfig(render)
	if err != nil {
		return nil, err
	}
	otelName, _, err := EnsureConfig(ctx, deps.Docker, "pmcluster_otel_config", otelYAML, in.Version)
	if err != nil {
		return nil, err
	}
	render.OTelConfigName = otelName
	traefikYAML, err := RenderTraefikDynamic(render)
	if err != nil {
		return nil, err
	}
	traefikName, _, err := EnsureConfig(ctx, deps.Docker, "pmcluster_traefik_dynamic", traefikYAML, in.Version)
	if err != nil {
		return nil, err
	}
	render.TraefikConfigName = traefikName
	edgeName, _, err := ensureEdgeConfig(ctx, deps.Docker, in.Version, render)
	if err != nil {
		return nil, err
	}
	_ = edgeName

	rendered, err := renderPlatformConfigs(render)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rendered))
	for name, content := range rendered {
		out[name] = string(content)
	}
	return out, nil
}
