package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/workflow"
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
	Docker   docker.Client
	Deployer StackDeployer
	Stdout   io.Writer

	// Provisioner, when set and the ingestion token is missing (an interrupted
	// `cluster up`), heals the store by provisioning the OO user + token before
	// rendering. Left nil (e.g. in tests) skips that network-dependent step.
	Provisioner *OpenObserveProvisioner
}

// Update re-provisions the OTel collector + Traefik dynamic configs and the
// TLS cert/key from the user-owned files under ConfigDir, pushing only what
// changed into Docker, and re-deploying only the stack(s) whose inputs moved.
//
// It never re-bootstraps credentials, never resets volumes, and never writes
// to the user's config files — those remain the source of truth. It is
// content-aware end to end: unchanged inputs are reused (no new versions, no
// redeploy) so a no-op update is just a report.
func Update(ctx context.Context, deps UpdateDeps, in UpdateInput) (*UpdateResult, error) {
	out := io.Discard
	if deps.Stdout != nil {
		out = deps.Stdout
	}
	res := &UpdateResult{}

	var (
		state        tlsState
		domain       string
		ooTokenPlain string
		render       RenderInput
	)

	wf := workflow.NewWorkflow(out)
	wf.Add("Preflight: Docker reachable, Swarm active", func(ctx context.Context) error {
		return Preflight(ctx, deps.Docker)
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

	wf.Add("Loading OpenObserve credentials + ingestion token", func(ctx context.Context) error {
		ooCred, err := deps.Store.GetCredential(ctx, "openobserve_admin")
		if err != nil {
			return fmt.Errorf("load openobserve_admin credential (run `cluster up` first): %w", err)
		}
		ooPass, err := deps.Cipher.Decrypt(ooCred.PasswordCiphertext)
		if err != nil {
			return fmt.Errorf("decrypt openobserve_admin password: %w", err)
		}
		ooTokenPlain, err = loadStoredIngestionToken(ctx, deps.Store, deps.Cipher)
		if err != nil {
			return err
		}
		if ooTokenPlain == "" && deps.Provisioner != nil {
			if _, _, err := deps.Provisioner.EnsureUserAndToken(ctx); err != nil {
				return err
			}
			ooTokenPlain, err = loadStoredIngestionToken(ctx, deps.Store, deps.Cipher)
			if err != nil {
				return err
			}
		}
		if ooTokenPlain == "" {
			ooTokenPlain = pendingIngestionToken
		}
		render = RenderInput{
			Domain:                    domain,
			OpenObserveAdminEmail:     ooCred.Username,
			OpenObserveAdminPassword:  string(ooPass),
			OpenObserveOrg:            "default",
			OpenObserveIngestionToken: ooTokenPlain,
			ACMEEmail:                 state.ACMEEmail,
			ConfigDir:                 in.ConfigDir,
			ConfigStore:               deps.Store,
			DataDir:                   filepath.Dir(in.ConfigDir),
			EdgeImage:                 EdgeImageFor(),
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
		certName, certCreated, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, "cert", state.CertPath)
		if err != nil {
			return fmt.Errorf("ensure cert secret: %w", err)
		}
		keyName, keyCreated, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, "key", state.KeyPath)
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

	wf.Add("Re-deploying stacks whose inputs changed", func(ctx context.Context) error {
		certChanged := res.CertCreated || res.KeyCreated
		if res.OTelCreated {
			fmt.Fprintf(out, "  ▶ %s changed → re-deploying %q\n", "OTel collector config", StackObservability)
			if err := deployStack(ctx, out, deps.Deployer, StackObservability, render); err != nil {
				return err
			}
			res.StacksDeployed = append(res.StacksDeployed, string(StackObservability))
		}
		if res.TraefikCreated || certChanged {
			reason := "Traefik dynamic config changed"
			if certChanged {
				reason = "certificate/key changed"
			}
			fmt.Fprintf(out, "  ▶ %s → re-deploying %q\n", reason, StackInfra)
			if err := deployStack(ctx, out, deps.Deployer, StackInfra, render); err != nil {
				return err
			}
			res.StacksDeployed = append(res.StacksDeployed, string(StackInfra))
		}
		if res.EdgeCreated {
			fmt.Fprintf(out, "  ▶ pmcluster-edge stack content changed (new image tag / config edit) → re-deploying\n")
			if err := deployStack(ctx, out, deps.Deployer, StackEdge, render); err != nil {
				return err
			}
			res.StacksDeployed = append(res.StacksDeployed, string(StackEdge))
			res.EdgeDeployed = true
		}
		if len(res.StacksDeployed) == 0 {
			fmt.Fprintf(out, "  ▶ No config or certificate changes — nothing to redeploy.\n")
		}
		return nil
	})

	wf.Add("Snapshotting rendered configs into the store", func(ctx context.Context) error {
		rendered, err := RenderClusterConfigs(ctx, deps, in)
		if err != nil {
			return fmt.Errorf("render platform configs for persistence: %w", err)
		}
		for name, content := range rendered {
			if err := deps.Store.SetRendered(ctx, name, content); err != nil && !errors.Is(err, store.ErrConfigNotFound) {
				return fmt.Errorf("store rendered config %s: %w", name, err)
			}
		}
		return nil
	})

	wf.Add("Cluster update complete.", func(ctx context.Context) error { return nil })

	if err := wf.Run(ctx); err != nil {
		return res, err
	}
	return res, nil
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
// YAML) without deploying anything. It mirrors Update's render construction and
// the idempotent TLS secret materialisation so the output matches what a
// `cluster update` would deploy. Keep in sync with Update.
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
	ooTokenPlain, err := loadStoredIngestionToken(ctx, deps.Store, deps.Cipher)
	if err != nil {
		return nil, err
	}
	if ooTokenPlain == "" {
		ooTokenPlain = pendingIngestionToken
	}
	render := RenderInput{
		Domain:                    domain,
		OpenObserveAdminEmail:     ooCred.Username,
		OpenObserveAdminPassword:  string(ooPass),
		OpenObserveOrg:            "default",
		OpenObserveIngestionToken: ooTokenPlain,
		ACMEEmail:                 state.ACMEEmail,
		ConfigDir:                 in.ConfigDir,
		ConfigStore:               deps.Store,
		DataDir:                   filepath.Dir(in.ConfigDir),
		EdgeImage:                 EdgeImageFor(),
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
		certName, _, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, "cert", state.CertPath)
		if err != nil {
			return nil, err
		}
		keyName, _, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, "key", state.KeyPath)
		if err != nil {
			return nil, err
		}
		render.CertSecretName, render.KeySecretName = certName, keyName
	}
	out := make(map[string]string)
	for _, s := range []stackName{StackInfra, StackObservability, StackBackup, StackEdge} {
		y, err := LoadComposeFile(s, render)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", s, err)
		}
		out[string(s)+"-stack"] = string(y)
	}
	otelYAML, err := RenderOTelCollectorConfig(render)
	if err != nil {
		return nil, fmt.Errorf("render otel-collector-config: %w", err)
	}
	out["otel-collector-config"] = string(otelYAML)
	traefikYAML, err := RenderTraefikDynamic(render)
	if err != nil {
		return nil, fmt.Errorf("render traefik-dynamic: %w", err)
	}
	out["traefik-dynamic"] = string(traefikYAML)
	return out, nil
}
