package cluster

import (
	"context"
	"fmt"
	"io"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// UpdateInput carries the config-dir + build version for `cluster update`.
// Unlike Up, UpdateInput carries no TLS/domain/credential fields — those are
// read back from the persisted install state so update needs no re-bootstrap.
type UpdateInput struct {
	ConfigDir string // ~/.pmcluster/config/ — user-owned config files
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
	Stdout   io.Writer // io.Discard in tests

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
	step := func(label string) { fmt.Fprintf(out, "▶ %s\n", label) }

	step("Preflight: Docker reachable, Swarm active")
	if err := Preflight(ctx, deps.Docker); err != nil {
		return nil, err
	}

	// Persisted install state tells us what to re-provision without asking.
	if deps.Store == nil {
		return nil, fmt.Errorf("update requires a store (config_dir must be initialised via `pmcluster init`)")
	}
	state, err := loadTLSSettings(ctx, deps.Store)
	if err != nil {
		return nil, err
	}
	domain := deps.Store.GetSettingDefault(ctx, settingDomain, "")
	if domain == "" {
		return nil, fmt.Errorf("no persisted domain found — run `cluster up` before `cluster update`")
	}

	// OpenObserve admin credential (email + decrypted password) from the store.
	ooCred, err := deps.Store.GetCredential(ctx, "openobserve_admin")
	if err != nil {
		return nil, fmt.Errorf("load openobserve_admin credential (run `cluster up` first): %w", err)
	}
	ooPass, err := deps.Cipher.Decrypt(ooCred.PasswordCiphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt openobserve_admin password: %w", err)
	}

	// The dedicated ingestion token (created via the OO API) is read from the
	// store. If missing (an interrupted `cluster up`), heal it via the
	// provisioner when one is injected, else fall back to the placeholder that
	// `up` seeded — keeping update content-consistent so a no-op stays a no-op.
	ooTokenPlain, err := loadStoredIngestionToken(ctx, deps.Store, deps.Cipher)
	if err != nil {
		return nil, err
	}
	if ooTokenPlain == "" && deps.Provisioner != nil {
		step("Provisioning missing OpenObserve user + ingestion token")
		if _, _, err := deps.Provisioner.EnsureUserAndToken(ctx); err != nil {
			return nil, err
		}
		ooTokenPlain, err = loadStoredIngestionToken(ctx, deps.Store, deps.Cipher)
		if err != nil {
			return nil, err
		}
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
		EdgeImage:                 EdgeImageFor(),
	}

	// TLS cert/key: content-aware re-apply from stored paths. Unchanged file
	// bytes reuse the current version (no churn, no Traefik restart).
	//
	// The discriminator MUST mirror what the infra-stack template keys on: it
	// renders the operator cert/key secret block whenever `.ACMEEmail` is empty
	// (the [[ if not .ACMEEmail ]] branch) and the Let's Encrypt resolver/volume
	// whenever `.ACMEEmail` is non-empty. Keying this off `state.Mode == "cert"`
	// instead diverged: a box whose mode wasn't persisted (or was "acme" with an
	// empty acme_email) took this branch's else, leaving CertSecretName/KeySecretName
	// empty while the template still rendered the cert block → dangling `:` in the
	// global secrets → invalid YAML on `cluster update`. So we gate on ACMEEmail:
	// empty ACMEEmail ALWAYS requires a cert/key (loaded from stored paths, or a
	// hard error if none are persisted), which is exactly what the template expects.
	if render.ACMEEmail != "" {
		// ACME: template renders the Let's Encrypt branch; no operator cert/key.
		step("TLS via Let's Encrypt (ACME email set) — no operator cert/key to re-apply")
	} else {
		if state.CertPath == "" || state.KeyPath == "" {
			// ACMEEmail empty means the template WILL emit the cert/key secret
			// block. Without persisted paths rendering empty names would produce
			// invalid YAML — fail loudly instead of emitting broken config.
			return res, fmt.Errorf("TLS is not ACME (no ACME email) but no cert/key paths are persisted — run `cluster up` with --cert/--key (or --acme-email) before `cluster update`")
		}
		step("Re-applying TLS cert/key from stored paths (skips if unchanged)")
		certName, certCreated, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, "cert", state.CertPath)
		if err != nil {
			return res, fmt.Errorf("ensure cert secret: %w", err)
		}
		keyName, keyCreated, err := EnsureVersionedSecretFromFile(ctx, deps.Docker, "key", state.KeyPath)
		if err != nil {
			return res, fmt.Errorf("ensure key secret: %w", err)
		}
		res.CertSecret, res.KeySecret = certName, keyName
		res.CertCreated, res.KeyCreated = certCreated, keyCreated
		render.CertSecretName, render.KeySecretName = certName, keyName
	}

	step("Rendering and provisioning OTel + Traefik configs (content-aware)")
	otelYAML, err := RenderOTelCollectorConfig(render)
	if err != nil {
		return res, err
	}
	otelName, otelCreated, err := EnsureConfig(ctx, deps.Docker, "pmcluster_otel_config", otelYAML, in.Version)
	if err != nil {
		return res, err
	}
	res.OTelConfig, res.OTelCreated = otelName, otelCreated
	render.OTelConfigName = otelName

	traefikYAML, err := RenderTraefikDynamic(render)
	if err != nil {
		return res, err
	}
	traefikName, traefikCreated, err := EnsureConfig(ctx, deps.Docker, "pmcluster_traefik_dynamic", traefikYAML, in.Version)
	if err != nil {
		return res, err
	}
	res.TraefikConfig, res.TraefikCreated = traefikName, traefikCreated
	render.TraefikConfigName = traefikName

	// Edge content fingerprint: the edge-stack.yml embeds a version-keyed image
	// tag, so a new EnsureConfig version means the rendered edge stack changed
	// (pmcluster version bump or operator config edit) and edge must re-deploy.
	edgeName, edgeCreated, err := ensureEdgeConfig(ctx, deps.Docker, in.Version, render)
	if err != nil {
		return res, err
	}
	res.EdgeConfig, res.EdgeCreated = edgeName, edgeCreated

	// Re-deploy ONLY the stacks whose inputs moved. The config/cert NAMES are
	// substituted into the compose, so a new version only takes effect when
	// its stack re-deploys (which force-restarts just that stack's consumers).
	// The backup stack is never touched by update.
	certChanged := res.CertCreated || res.KeyCreated
	if otelCreated {
		step(fmt.Sprintf("OTel collector config changed → re-deploying %q", StackObservability))
		if err := deployStack(ctx, out, deps.Deployer, StackObservability, render); err != nil {
			return res, err
		}
		res.StacksDeployed = append(res.StacksDeployed, string(StackObservability))
	}
	if traefikCreated || certChanged {
		reason := "Traefik dynamic config changed"
		if certChanged {
			reason = "certificate/key changed"
		}
		step(fmt.Sprintf("%s → re-deploying %q", reason, StackInfra))
		if err := deployStack(ctx, out, deps.Deployer, StackInfra, render); err != nil {
			return res, err
		}
		res.StacksDeployed = append(res.StacksDeployed, string(StackInfra))
	}
	if edgeCreated {
		step("pmcluster-edge stack content changed (new image tag / config edit) → re-deploying")
		if err := deployStack(ctx, out, deps.Deployer, StackEdge, render); err != nil {
			return res, err
		}
		res.StacksDeployed = append(res.StacksDeployed, string(StackEdge))
		res.EdgeDeployed = true
	}

	if len(res.StacksDeployed) == 0 {
		step("No config or certificate changes — nothing to redeploy.")
	}
	step("Cluster update complete.")
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
