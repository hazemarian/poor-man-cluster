package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/logger"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

var clusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Manage the cluster lifecycle (bootstrap, status, teardown)",
}

var clusterUpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "Re-provision OTel + Traefik configs (and cert/key) from your config files",
	Long: `Targeted re-provisioning for config/cert changes — unlike ` + "`up`" + ` it does
NOT re-run a full bring-up: no credential bootstrap, no volume reset, no
full-stack redeploy.

  - Syncs the six platform config templates into the store + ~/.pmcluster/config/
    (the store is the source of truth; stale rows are refreshed to the current
    build, and your edits at the current version are preserved + mirrored)
  - Re-reads otel-collector-config.yml + traefik-dynamic.yml
  - Content-aware: unchanged inputs reuse the current Docker config version
  - Re-applies cert/key from the stored TLS paths when those files changed
  - Re-deploys observability (OTel changed), infra (Traefik/cert changed,
    and removes services that dropped out of the compose — no more drift),
    and edge (new image tag or config edit)

Run this after editing a config, renewing your certificate, or upgrading the
pmcluster binary.`,
	RunE: runClusterUpdate,
}

var clusterUpCmd = &cobra.Command{
	Use:   "up",
	Short: "Bring the cluster up: preflight, secrets, networks, configs, stacks",
	Long: `On a manager with Docker installed and Swarm initialised:

  - Preflight (Docker reachable, Swarm active, this node is a manager)
  - Ensure overlay networks (traefik-net, monitoring-net)
  - Configure TLS:
      --acme-email <you@host>   Let's Encrypt via Traefik HTTP-01
                                (DNS must point here AND port 80 must be reachable)
      --cert <pem> --key <pem>  Operator-supplied certificate
  - Generate random bootstrap credentials for traefik/openobserve/edge
    (encrypted in SQLite + mirrored to Swarm secrets; existing values preserved)
  - Render OTel + Traefik dynamic configs in-process; ship as Docker configs
  - Deploy infra/observability/backup stacks via 'docker stack deploy'

Idempotent: re-running reconciles, never destroys existing state.`,
	RunE: runClusterUp,
}

var clusterResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Non-destructive rebuild of the Swarm from the local store",
	Long: `Rebuilds the Swarm side of the cluster FROM the local SQLite store — the DB
is the desired state / source of truth and the Swarm is derived.

  - Re-ensures overlay networks (traefik-net, monitoring-net)
  - Re-materializes every managed credential's Swarm secret from the DB
  - Rebuilds missing swarm configs/secrets from the DB index
  - Redeploys every platform stack (force) regardless of rendered hash
  - Re-applies storage-node labels

Use this after the Swarm was wiped (docker swarm leave --force + swarm init)
while the local store survived, or any time the Swarm must be rebuilt from the
DB. It never rotates credentials and never resets volumes.

  --restore <archive>   extract a ` + "`cluster down --purge`" + ` backup tarball
                        into the data dir (restores data.db + .encryption_key)
                        BEFORE rebuilding.`,
	RunE: runClusterReset,
}

func init() {
	clusterUpCmd.Flags().String("domain", "", "base domain for the cluster (e.g. example.com)")
	clusterUpCmd.Flags().String("acme-email", "", "Let's Encrypt account email — Traefik issues + renews certs via HTTP-01")
	clusterUpCmd.Flags().String("cert", "", "path to TLS certificate (PEM) — alternative to --acme-email")
	clusterUpCmd.Flags().String("key", "", "path to TLS private key (PEM) — alternative to --acme-email")
	clusterUpCmd.Flags().String("openobserve-email", "", "OpenObserve admin email (becomes admin login)")
	clusterUpCmd.Flags().String("traefik-admin-user", "admin", "username for the Traefik dashboard basic-auth")
	clusterUpCmd.Flags().Bool("force-tls-mode", false, "allow switching TLS mode on an already-installed cluster (cert <-> acme)")
	clusterUpCmd.Flags().String("swarm-advertise-addr", "", "advertise address passed to `docker swarm init` when this node is not yet in a Swarm (default: auto-detected node IP)")
	clusterUpCmd.Flags().Bool("tailscale", false, "first node: bring this node onto a private WireGuard tailnet (tailscale CLI required) and init the Swarm advertising the tailnet IPv4")
	clusterUpCmd.Flags().String("tailscale-auth-key", "", "tailnet auth key for 'tailscale up' (default: $PMCLUSTER_TAILSCALE_AUTH_KEY)")

	clusterDownCmd.Flags().Bool("yes", false, "skip confirmation prompt")
	clusterDownCmd.Flags().Bool("purge", false, "also remove pmcluster-managed secrets, configs, networks, and the local store (backing it up first)")

	clusterResetCmd.Flags().String("restore", "", "restore a purge-backup tarball into the data dir before rebuilding")

	clusterCmd.AddCommand(clusterUpCmd, clusterUpdateCmd, clusterStatusCmd, clusterDownCmd, clusterResetCmd)
}

// clusterState classifies what the local store says about this node.
type clusterState int

const (
	// stateProvisioned: the local store holds the cluster's settings.
	stateProvisioned clusterState = iota
	// stateStandby: no local control-plane state, but the node is a member of an
	// existing Swarm. A manager that joined but was never promoted keeps an
	// intentionally empty store — the control plane lives in the Raft snapshot
	// and is restored when it is promoted — so this is not a fresh box.
	stateStandby
	// stateFresh: no local state and no Swarm — a fresh box that needs `setup`.
	stateFresh
)

// classifyLocalCluster decides whether `cluster update` / `cluster reset` should
// re-provision this node, stand by, or guide the operator into the setup wizard.
// A missing local `domain` on its own does not mean "no cluster": a manager that
// joined but was never promoted has none, and treating it as a fresh box sent
// install.sh's auto `cluster update` into the interactive wizard, which then
// failed with "domain is required" (BUG-022).
func classifyLocalCluster(ctx context.Context, st *store.Store, dc runtime.Client) clusterState {
	if st.GetSettingDefault(ctx, cluster.SettingDomain(), "") != "" {
		return stateProvisioned
	}
	if dc != nil {
		if id, err := dc.SwarmID(ctx); err == nil && id != "" {
			return stateStandby
		}
	}
	return stateFresh
}

// probeDocker opens a best-effort Docker client used only to classify this node;
// a failure (a fresh box with no daemon) is not an error.
func probeDocker() (runtime.Client, func()) {
	dc, err := docker.New()
	if err != nil {
		return nil, func() {}
	}
	return dc, func() { _ = dc.Close() }
}

func runClusterUpdate(cmd *cobra.Command, _ []string) error {
	defer initCLITelemetry()()

	ctx := cmd.Context()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return fmt.Errorf("data directory not initialised at %s — run `pmcluster init` first", cfg.DataDir)
	}

	log, logCloser, err := logger.New(logger.Options{
		LogsDir: cfg.LogsDir(),
		Level:   cfg.LogLevel,
		Console: false,
	})
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logCloser.Close() }()
	log.Info().Msg("cluster update: starting")
	defer log.Info().Msg("cluster update: finished")

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	// No persisted cluster state? Either this is a fresh box (guide the operator
	// into the setup wizard) or a manager that joined but was never promoted:
	// its store is intentionally empty and the control plane is restored from
	// the Raft snapshot on promotion, so there is nothing to update on it
	// (BUG-022).
	if st.GetSettingDefault(ctx, cluster.SettingDomain(), "") == "" {
		probe, closeProbe := probeDocker()
		defer closeProbe()
		if classifyLocalCluster(ctx, st, probe) == stateStandby {
			fmt.Fprintln(cmd.OutOrStdout(), "This node is a Swarm manager with no local control-plane state yet (it has not been promoted — the store is restored from the Raft snapshot on promotion). The cluster exists; nothing to update here.")
			return nil
		}
		fmt.Fprintln(cmd.OutOrStdout(), "No cluster configuration found — starting the interactive setup wizard (`pmcluster setup`).")
		return runSetup(cmd, nil)
	}

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		return fmt.Errorf("open encryption key: %w", err)
	}

	dc, err := docker.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = dc.Close() }()

	deployer := cluster.NewDockerCLIDeployer(cmd.OutOrStdout())

	res, err := cluster.NewService().Update(ctx, cluster.UpdateDeps{
		Store:    st,
		Cipher:   cipher,
		Docker:   dc,
		Deployer: deployer,
		Stdout:   cmd.OutOrStdout(),
	}, cluster.UpdateInput{
		ConfigDir: cfg.ConfigDir(),
		Version:   buildinfo.Version,
	})
	if err != nil {
		return err
	}
	printUpdateResult(cmd.OutOrStdout(), res)

	// The daemon is managed by the CLI now (not install.sh): make sure it is
	// installed + running with the current binary.
	if err := ensureDaemonRunning(cmd.OutOrStdout()); err != nil {
		return fmt.Errorf("ensure daemon running: %w", err)
	}
	return nil
}

func printUpdateResult(out io.Writer, res *cluster.UpdateResult) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "✔ pmcluster cluster update complete.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  OTel collector config  : %s %s\n", res.OTelConfig, changedMarker(res.OTelCreated))
	fmt.Fprintf(out, "  Traefik dynamic config : %s %s\n", res.TraefikConfig, changedMarker(res.TraefikCreated))
	if res.CertSecret != "" {
		fmt.Fprintf(out, "  TLS cert secret        : %s %s\n", res.CertSecret, changedMarker(res.CertCreated))
		fmt.Fprintf(out, "  TLS key secret         : %s %s\n", res.KeySecret, changedMarker(res.KeyCreated))
	}
	fmt.Fprintf(out, "  Stacks re-deployed     : %v\n", res.StacksDeployed)
	if len(res.StacksDeployed) == 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  (no config or certificate changes — nothing to redeploy)")
	}
}

// changedMarker renders a small annotation for a changed/reused item.
func changedMarker(changed bool) string {
	if changed {
		return "[rotated]"
	}
	return "[unchanged]"
}

func runClusterReset(cmd *cobra.Command, _ []string) error {
	defer initCLITelemetry()()

	ctx := cmd.Context()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Optional restore: extract a purge backup into the data dir BEFORE
	// opening the store (it restores data.db + .encryption_key).
	if restore, _ := cmd.Flags().GetString("restore"); restore != "" {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return fmt.Errorf("create data dir: %w", err)
		}
		if err := cluster.RestoreStoreBackup(cfg.DataDir, restore); err != nil {
			return fmt.Errorf("restore backup %s: %w", restore, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "✔ restored store backup %s into %s\n", restore, cfg.DataDir)
	}

	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return fmt.Errorf("data directory not initialised at %s — run `pmcluster init` first (or pass --restore <archive>)", cfg.DataDir)
	}

	log, logCloser, err := logger.New(logger.Options{
		LogsDir: cfg.LogsDir(),
		Level:   cfg.LogLevel,
		Console: false,
	})
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logCloser.Close() }()
	log.Info().Msg("cluster reset: starting")
	defer log.Info().Msg("cluster reset: finished")

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	// No persisted cluster state? Either a fresh box (guide the operator into the
	// setup wizard) or a manager that was never promoted, whose store is
	// intentionally empty (BUG-022).
	if st.GetSettingDefault(ctx, cluster.SettingDomain(), "") == "" {
		probe, closeProbe := probeDocker()
		defer closeProbe()
		if classifyLocalCluster(ctx, st, probe) == stateStandby {
			fmt.Fprintln(cmd.OutOrStdout(), "This node is a Swarm manager with no local control-plane state yet (it has not been promoted — the store is restored from the Raft snapshot on promotion). Rebuild from the leader with `pmcluster cluster reset` there, or let this node be promoted.")
			return nil
		}
		fmt.Fprintln(cmd.OutOrStdout(), "No cluster configuration found — starting the interactive setup wizard (`pmcluster setup`).")
		return runSetup(cmd, nil)
	}

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		return fmt.Errorf("open encryption key: %w", err)
	}

	dc, err := docker.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = dc.Close() }()

	deployer := cluster.NewDockerCLIDeployer(cmd.OutOrStdout())

	res, err := cluster.NewService().Update(ctx, cluster.UpdateDeps{
		Store:    st,
		Cipher:   cipher,
		Docker:   dc,
		Deployer: deployer,
		Stdout:   cmd.OutOrStdout(),
	}, cluster.UpdateInput{
		ConfigDir: cfg.ConfigDir(),
		Version:   buildinfo.Version,
		Force:     true,
	})
	if err != nil {
		return err
	}
	printResetResult(cmd.OutOrStdout(), res)

	// Rebuild is done — make sure the daemon is installed + running.
	if err := ensureDaemonRunning(cmd.OutOrStdout()); err != nil {
		return fmt.Errorf("ensure daemon running: %w", err)
	}
	return nil
}

// printResetResult summarises what a `cluster reset` created/redeployed.
func printResetResult(out io.Writer, res *cluster.UpdateResult) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "✔ pmcluster cluster reset complete (Swarm rebuilt from the DB).")
	fmt.Fprintln(out)
	if res.OTelConfig != "" {
		fmt.Fprintf(out, "  OTel collector config  : %s\n", res.OTelConfig)
	}
	if res.TraefikConfig != "" {
		fmt.Fprintf(out, "  Traefik dynamic config : %s\n", res.TraefikConfig)
	}
	if res.CertSecret != "" {
		fmt.Fprintf(out, "  TLS cert secret        : %s\n", res.CertSecret)
	}
	fmt.Fprintf(out, "  Stacks re-deployed     : %v\n", res.StacksDeployed)
	fmt.Fprintln(out)
}

func runClusterUp(cmd *cobra.Command, _ []string) error {
	defer initCLITelemetry()()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return fmt.Errorf("data directory not initialised at %s — run `pmcluster init` first", cfg.DataDir)
	}

	in := cluster.UpInput{}
	in.Version = buildinfo.Version
	in.Domain, _ = cmd.Flags().GetString("domain")
	in.ACMEEmail, _ = cmd.Flags().GetString("acme-email")
	in.CertPath, _ = cmd.Flags().GetString("cert")
	in.KeyPath, _ = cmd.Flags().GetString("key")
	in.OpenObserveAdminEmail, _ = cmd.Flags().GetString("openobserve-email")
	in.TraefikAdminUser, _ = cmd.Flags().GetString("traefik-admin-user")
	in.ForceTLSMode, _ = cmd.Flags().GetBool("force-tls-mode")
	in.ConfigDir = cfg.ConfigDir()
	in.SwarmAdvertiseAddr, _ = cmd.Flags().GetString("swarm-advertise-addr")

	// M3 — optional tailnet for a first node: when the operator opts in and did
	// not already pin --swarm-advertise-addr, bring this node onto the tailnet
	// and let `docker swarm init` advertise the tailnet IPv4. Explicit opt-in,
	// so a tailnet failure fails the up (silent fallback to a public IP would
	// defeat the purpose).
	if ts, _ := cmd.Flags().GetBool("tailscale"); ts && in.SwarmAdvertiseAddr == "" {
		authKey, _ := cmd.Flags().GetString("tailscale-auth-key")
		if authKey == "" {
			authKey = tailscaleAuthKeyFromEnv()
		}
		advertise, err := tailscaleReady(cmd.Context(), cmd.OutOrStdout(), authKey, "")
		if err != nil {
			return err
		}
		in.SwarmAdvertiseAddr = advertise
	}

	return runUp(cmd, cfg, in)
}

// clusterUpHasInput reports whether the operator supplied any provisioning
// input for `cluster up`, either via flags (--domain/--acme-email/--cert/--key)
// or from previously persisted store settings. When neither exists the command
// falls back to the interactive setup wizard.
func clusterUpHasInput(cmd *cobra.Command, st *store.Store, ctx context.Context) bool {
	if cmd.Flags().Changed("domain") ||
		cmd.Flags().Changed("acme-email") ||
		cmd.Flags().Changed("cert") ||
		cmd.Flags().Changed("key") {
		return true
	}
	return st.GetSettingDefault(ctx, "domain", "") != ""
}

// runUp executes the `cluster up` workflow with an explicit UpInput: opens the
// store / cipher / docker / deployer, fills any empty UpInput fields from the
// stored settings (so the `pmcluster setup` wizard only has to persist the
// settings it wants Up to read), runs the workflow and prints the result.
// Shared by runClusterUp and the setup wizard's fresh-install handoff.
func runUp(cmd *cobra.Command, cfg *config.Config, in cluster.UpInput) error {
	defer initCLITelemetry()()

	ctx := cmd.Context()

	log, logCloser, err := logger.New(logger.Options{
		LogsDir: cfg.LogsDir(),
		Level:   cfg.LogLevel,
		Console: false,
	})
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logCloser.Close() }()
	_ = logger.Sweep(cfg.LogsDir(), time.Now())
	log.Info().Msg("cluster up: starting")
	defer log.Info().Msg("cluster up: finished")

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	// No provisioning input at all (no flags, nothing stored)? Drop into the
	// interactive setup wizard instead of failing cryptically. `cluster up`
	// with env/flags supplied proceeds as usual.
	if !clusterUpHasInput(cmd, st, ctx) {
		fmt.Fprintln(cmd.OutOrStdout(), "No cluster configuration found — starting the interactive setup wizard (`pmcluster setup`).")
		return runSetup(cmd, nil)
	}

	// Fill fields from stored settings when not already set (e.g. after the
	// `pmcluster setup` wizard persisted them).
	if in.Domain == "" {
		in.Domain = st.GetSettingDefault(ctx, "domain", "")
	}
	if in.OpenObserveAdminEmail == "" {
		in.OpenObserveAdminEmail = st.GetSettingDefault(ctx, "oo_admin_email", "")
	}
	if in.TraefikAdminUser == "" {
		in.TraefikAdminUser = st.GetSettingDefault(ctx, cluster.SettingTraefikAdminUser(), "admin")
	}
	if in.VolumeRoot == "" {
		in.VolumeRoot = st.GetSettingDefault(ctx, cluster.SettingVolumeRoot(), "")
	}

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		return fmt.Errorf("open encryption key: %w", err)
	}

	dc, err := docker.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = dc.Close() }()

	// First node: initialise the Swarm itself when it is not already part of
	// one. The operator's hostname choice was applied by the setup wizard (or
	// --hostname) before this point, so `docker swarm init` records the
	// intended name — keeping `placement: <hostname>` pins valid.
	if err := ensureSwarmInitialized(ctx, cmd.OutOrStdout(), in.SwarmAdvertiseAddr); err != nil {
		return err
	}

	deployer := cluster.NewDockerCLIDeployer(cmd.OutOrStdout())

	res, err := cluster.NewService().Up(ctx, cluster.UpDeps{
		Store:    st,
		Cipher:   cipher,
		Docker:   dc,
		Deployer: deployer,
		Stdout:   cmd.OutOrStdout(),
	}, in)
	if err != nil {
		return err
	}

	printUpResult(cmd.OutOrStdout(), in, res)
	printNodePins(cmd.OutOrStdout(), ctx, dc)

	// The daemon is managed by the CLI now (not install.sh): make sure it is
	// installed + running with the current binary.
	if err := ensureDaemonRunning(cmd.OutOrStdout()); err != nil {
		return fmt.Errorf("ensure daemon running: %w", err)
	}
	return nil
}

// printNodePins lists the Swarm nodes and their hostnames so the operator can
// pin stateful services to a specific node via `placement: <hostname>`.
func printNodePins(out io.Writer, ctx context.Context, dc runtime.Client) {
	nodes, err := dc.NodeList(ctx)
	if err != nil || len(nodes) == 0 {
		return // Node listing is informational; never fail the command on it.
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Swarm nodes (use the hostname in `placement:` to pin a stateful service):")
	for _, n := range nodes {
		role := "worker"
		if n.IsLeader {
			role = "manager (leader)"
		} else if n.Role == "manager" {
			role = "manager"
		}
		fmt.Fprintf(out, "    - %s   [%s]\n", n.Hostname, role)
	}
	fmt.Fprintln(out)
}

func printUpResult(out io.Writer, in cluster.UpInput, res *cluster.UpResult) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "🎉 pmcluster cluster up complete.")
	fmt.Fprintln(out)
	if len(res.NewNetworks) > 0 {
		fmt.Fprintf(out, "  Networks created : %v\n", res.NewNetworks)
	}
	if len(res.NewSecrets) > 0 {
		fmt.Fprintf(out, "  Secrets created  : %v\n", res.NewSecrets)
	}
	if len(res.NewConfigs) > 0 {
		fmt.Fprintf(out, "  Configs created  : %v\n", res.NewConfigs)
	}
	fmt.Fprintf(out, "  Stacks deployed  : %v\n", res.StacksDeployed)
	fmt.Fprintln(out)

	if len(res.BootstrapCredentials) > 0 {
		fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════")
		fmt.Fprintln(out, "🔑 BOOTSTRAP CREDENTIALS — save these somewhere safe")
		fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════")
		fmt.Fprintln(out)
		order := []string{"traefik_dashboard", "openobserve_admin"}
		for _, name := range order {
			c := res.BootstrapCredentials[name]
			if c == nil {
				continue
			}
			marker := "(existing)"
			if c.NewlyCreated {
				marker = "(NEW)"
			}
			if c.UsernameChanged {
				marker = "(username updated)"
			}
			fmt.Fprintf(out, "   %s %s\n", name, marker)
			fmt.Fprintf(out, "     user:     %s\n", c.Username)
			fmt.Fprintf(out, "     password: %s\n", c.Password)
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out, "   Retrieve later: pmcluster credentials show <name>")
		fmt.Fprintln(out, "   Audit log:      pmcluster logs --tail=200")
		fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════")
		fmt.Fprintln(out)
	}

	fmt.Fprintf(out, "Dashboards (once DNS resolves):\n")
	fmt.Fprintf(out, "  Traefik     https://traefik.%s\n", in.Domain)
	fmt.Fprintf(out, "  OpenObserve https://observ.%s\n", in.Domain)
	fmt.Fprintf(out, "  pmcluster   https://pmcluster.%s   (after `pmcluster serve` is supervised)\n", in.Domain)
	fmt.Fprintln(out)
}

var clusterStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show cluster + control-plane health summary",
	RunE:  runClusterStatus,
}

var clusterDownCmd = &cobra.Command{
	Use:   "down",
	Short: "Tear down the bundled stacks (infra/observability/backup)",
	Long: `Removes the infra/observability/backup stacks. Secrets, networks, and
SQLite state are preserved unless --purge is passed.

  --purge   also remove pmcluster-managed Swarm secrets, Docker configs,
            and the two overlay networks. Does NOT delete ~/.pmcluster
            (encryption key + SQLite); rm that directory manually if you
            want a fully clean slate.

  --yes     skip the confirmation prompt (useful in scripts).`,
	RunE: runClusterDown,
}

func runClusterStatus(cmd *cobra.Command, _ []string) error {
	dc, err := docker.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = dc.Close() }()

	report, err := cluster.NewService().Status(cmd.Context(), dc)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Cluster status:")
	fmt.Fprintf(out, "  Node name      : %s\n", report.NodeName)
	fmt.Fprintf(out, "  Server version : %s\n", report.ServerVersion)
	fmt.Fprintf(out, "  Swarm state    : %s\n", report.SwarmState)
	fmt.Fprintf(out, "  Manager        : %v\n", report.IsManager)
	fmt.Fprintf(out, "  Nodes / Mgrs   : %d / %d\n", report.NodeCount, report.ManagerCount)
	fmt.Fprintln(out)
	if report.Preflight != nil {
		fmt.Fprintln(out, "Preflight: ❌")
		fmt.Fprintln(out, report.Preflight.Error())
		return report.Preflight
	}
	fmt.Fprintln(out, "Preflight: ✅ ready for `pmcluster cluster up`")
	return nil
}

func runClusterDown(cmd *cobra.Command, _ []string) error {
	yes, _ := cmd.Flags().GetBool("yes")
	purge, _ := cmd.Flags().GetBool("purge")

	if !yes {
		fmt.Fprintf(cmd.OutOrStdout(),
			"This will remove the infra/observability/backup stacks%s.\nRe-run with --yes to confirm.\n",
			ternary(purge, " AND purge pmcluster-managed secrets, configs, overlay networks, and the local store", ""),
		)
		return fmt.Errorf("not confirmed")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	out := cmd.OutOrStdout()

	// Purge must stop the daemon FIRST (best-effort, systemd-only) so it cannot
	// resurrect swarm resources while we remove them.
	if purge {
		stopDaemon(out)
	}

	dc, err := docker.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = dc.Close() }()

	deployer := cluster.NewDockerCLIDeployer(out)

	downInput := cluster.DownInput{Purge: purge}
	if purge {
		downInput.StoreDir = cfg.DataDir
	}

	res, err := cluster.NewService().Down(cmd.Context(), cluster.DownDeps{
		Docker:   dc,
		Deployer: deployer,
		Stdout:   out,
	}, downInput)
	if err != nil {
		return err
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Removed:")
	fmt.Fprintf(out, "  Stacks   : %v\n", res.StacksRemoved)
	if purge {
		fmt.Fprintf(out, "  Secrets  : %v\n", res.SecretsRemoved)
		fmt.Fprintf(out, "  Configs  : %v\n", res.ConfigsRemoved)
		fmt.Fprintf(out, "  Networks : %v\n", res.NetworksRemoved)
		if res.StoreBackupPath != "" {
			fmt.Fprintln(out)
			fmt.Fprintf(out, "  Store    : deleted (backup kept at %s)\n", res.StoreBackupPath)
			fmt.Fprintf(out, "  Restore  : pmcluster cluster reset --restore %s\n", res.StoreBackupPath)
		}
	}
	return nil
}

func ternary[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}
