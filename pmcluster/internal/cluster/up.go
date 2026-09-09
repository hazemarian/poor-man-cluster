package cluster

import (
	"context"
	"fmt"
	"io"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
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
	ForceTLSMode          bool   // allow switching tls_mode on an already-installed cluster
	TraefikAdminUser      string // defaults to "admin"
	OpenObserveAdminEmail string
	ConfigDir             string // ~/.pmcluster/config/ — user-editable templates
	Version               string // build version (e.g. "v0.2.12") — tagged onto Docker configs
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
	Docker   docker.Client
	Deployer StackDeployer
	Stdout   io.Writer // io.Discard in tests
}

// Up brings the cluster up end-to-end. Order matters: preflight →
// networks → TLS secrets → bootstrap creds → render configs → deploy
// stacks (infra → observability → backup).
func Up(ctx context.Context, deps UpDeps, in UpInput) (*UpResult, error) {
	// Load persisted install state so an idempotent re-run can reuse the
	// originally-chosen TLS mode and cert/key paths instead of re-minting.
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
	out := io.Discard
	if deps.Stdout != nil {
		out = deps.Stdout
	}

	res := &UpResult{}
	step := func(label string) { fmt.Fprintf(out, "▶ %s\n", label) }

	step("Preflight: Docker reachable, Swarm active, this node is a manager")
	if err := Preflight(ctx, deps.Docker); err != nil {
		return nil, err
	}

	step("Ensuring overlay networks")
	created, err := EnsureBundledNetworks(ctx, deps.Docker)
	if err != nil {
		return res, fmt.Errorf("ensure networks: %w", err)
	}
	res.NewNetworks = created

	var certSecret, keySecret string
	if in.ACMEEmail != "" {
		step("TLS via Let's Encrypt (Traefik HTTP-01) — port 80 must be reachable from the internet")
	} else {
		step("Loading TLS cert/key into Swarm secrets")
		var certCreated, keyCreated bool
		certSecret, certCreated, err = EnsureVersionedSecretFromFile(ctx, deps.Docker, "cert", in.CertPath)
		if err != nil {
			return res, fmt.Errorf("ensure cert secret: %w", err)
		}
		keySecret, keyCreated, err = EnsureVersionedSecretFromFile(ctx, deps.Docker, "key", in.KeyPath)
		if err != nil {
			return res, fmt.Errorf("ensure key secret: %w", err)
		}
		if certCreated || keyCreated {
			res.NewSecrets = append(res.NewSecrets, certSecret, keySecret)
		}
	}

	step("Bootstrapping managed credentials (Traefik / Portainer / OpenObserve)")
	credMgr := &CredentialsManager{
		Store:  deps.Store,
		Cipher: deps.Cipher,
		Docker: deps.Docker,
	}
	creds, err := credMgr.Bootstrap(ctx, BootstrapInput{
		TraefikAdminUser:      in.TraefikAdminUser,
		OpenObserveAdminEmail: in.OpenObserveAdminEmail,
	})
	if err != nil {
		return res, fmt.Errorf("bootstrap credentials: %w", err)
	}
	res.BootstrapCredentials = creds
	for name, c := range creds {
		if name == "openobserve_token" {
			// Internal ingestion credential — not a login; keep it out of
			// the human-facing bootstrap summary.
			continue
		}
		switch {
		case c.NewlyCreated && c.SwarmSecretCreated:
			res.NewSecrets = append(res.NewSecrets, c.SwarmSecretName)
			fmt.Fprintf(out, "  ✓ %-20s newly created (swarm secret %s)\n", name, c.SwarmSecretName)
		case c.NewlyCreated && !c.SwarmSecretCreated:
			// "Lost DB, kept Swarm" recovery: pmcluster minted a new
			// password but a Swarm secret with that name already existed.
			// Store and Swarm now disagree.
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

	// When the OpenObserve admin email changes, the data volume must be
	// reset so ZO_ROOT_USER_* env vars take effect on the next boot.
	// OpenObserve only reads those env vars on first run; after that the
	// hashed password lives in the volume and ignores env updates.
	if obsCred := creds["openobserve_admin"]; obsCred != nil && obsCred.UsernameChanged {
		fmt.Fprintf(out, "  ⚠ OpenObserve email changed → resetting data volume so new credentials take effect\n")
		if err := deps.Docker.VolumeRemove(ctx, "observability_openobserve_data"); err != nil {
			return res, fmt.Errorf("reset openobserve data volume: %w (manual: docker volume rm observability_openobserve_data)", err)
		}
	}

	step("Rendering and creating Docker configs (OTel pipeline, Traefik dynamic)")
	openobsCred := creds["openobserve_admin"]
	if openobsCred == nil {
		return res, fmt.Errorf("internal: openobserve_admin credential missing after bootstrap")
	}
	ooToken := creds["openobserve_token"]
	if ooToken == nil {
		return res, fmt.Errorf("internal: openobserve_token credential missing after bootstrap")
	}
	render := RenderInput{
		Domain:                   in.Domain,
		OpenObserveAdminEmail:    openobsCred.Username,
		OpenObserveAdminPassword: openobsCred.Password,
		OpenObserveRootToken:     ooToken.Password,
		ACMEEmail:                in.ACMEEmail,
		ConfigDir:                in.ConfigDir,
		CertSecretName:           certSecret,
		KeySecretName:            keySecret,
	}

	otelYAML, err := RenderOTelCollectorConfig(render)
	if err != nil {
		return res, err
	}
	otelConfigName, otelConfigCreated, err := EnsureConfig(ctx, deps.Docker, "pmcluster_otel_config", otelYAML, in.Version)
	if err != nil {
		return res, err
	}
	if otelConfigCreated {
		res.NewConfigs = append(res.NewConfigs, otelConfigName)
	}
	render.OTelConfigName = otelConfigName

	traefikYAML, err := RenderTraefikDynamic(render)
	if err != nil {
		return res, err
	}
	traefikConfigName, traefikConfigCreated, err := EnsureConfig(ctx, deps.Docker, "pmcluster_traefik_dynamic", traefikYAML, in.Version)
	if err != nil {
		return res, err
	}
	if traefikConfigCreated {
		res.NewConfigs = append(res.NewConfigs, traefikConfigName)
	}
	render.TraefikConfigName = traefikConfigName

	step("Deploying stacks (infra → observability → backup)")
	for _, s := range []stackName{StackInfra, StackObservability, StackBackup} {
		composeYAML, err := LoadComposeFile(s, render)
		if err != nil {
			return res, fmt.Errorf("load compose for %s: %w", s, err)
		}
		fmt.Fprintf(out, "  ▶ docker stack deploy %s\n", s)
		if err := deps.Deployer.DeployStack(ctx, string(s), composeYAML); err != nil {
			return res, fmt.Errorf("deploy %s: %w", s, err)
		}
		res.StacksDeployed = append(res.StacksDeployed, string(s))
	}

	step("Waiting for all services to become healthy")
	if err := WaitHealthyStacks(ctx, deps.Docker, out); err != nil {
		return res, fmt.Errorf("health check: %w", err)
	}

	// Persist the install inputs so `up` re-runs and `cluster update` can
	// reuse them without re-passing/re-deriving TLS state.
	if err := persistInstallState(ctx, deps, in); err != nil {
		return res, err
	}

	step("Cluster up complete.")
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
