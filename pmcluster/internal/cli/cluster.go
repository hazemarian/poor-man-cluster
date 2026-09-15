package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/logger"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/openobserve"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// ooProvisioner builds the OpenObserve user+token provisioner for `up`/`update`.
// It reaches OpenObserve over the public edge (https://observ.<domain>) from
// the manager host.
func ooProvisioner(st *store.Store, cipher *credentials.Cipher, out io.Writer, domain string) *cluster.OpenObserveProvisioner {
	return &cluster.OpenObserveProvisioner{
		Store:  st,
		Cipher: cipher,
		Client: openobserve.NewClient("https://observ."+domain, "default"),
		Org:    "default",
		Stdout: out,
	}
}

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

  - Re-reads ~/.pmcluster/config/otel-collector-config.yml + traefik-dynamic.yml
    (your files are the source of truth; NEVER overwritten here)
  - Content-aware: unchanged files reuse the current Docker config version
  - Re-applies cert/key from the stored TLS paths when those files changed
  - Re-deploys only observability (when the OTel config changed) and infra
    (when the Traefik config or cert changed)

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
  - Generate random bootstrap credentials for traefik/portainer/openobserve
    (encrypted in SQLite + mirrored to Swarm secrets; existing values preserved)
  - Render OTel + Traefik dynamic configs in-process; ship as Docker configs
  - Deploy infra/observability/backup stacks via 'docker stack deploy'

Idempotent: re-running reconciles, never destroys existing state.`,
	RunE: runClusterUp,
}

func init() {
	clusterUpCmd.Flags().String("domain", "", "base domain for the cluster (e.g. example.com)")
	clusterUpCmd.Flags().String("acme-email", "", "Let's Encrypt account email — Traefik issues + renews certs via HTTP-01")
	clusterUpCmd.Flags().String("cert", "", "path to TLS certificate (PEM) — alternative to --acme-email")
	clusterUpCmd.Flags().String("key", "", "path to TLS private key (PEM) — alternative to --acme-email")
	clusterUpCmd.Flags().String("openobserve-email", "", "OpenObserve admin email (becomes admin login)")
	clusterUpCmd.Flags().String("traefik-admin-user", "admin", "username for the Traefik dashboard basic-auth")
	clusterUpCmd.Flags().Bool("force-tls-mode", false, "allow switching TLS mode on an already-installed cluster (cert <-> acme)")

	clusterDownCmd.Flags().Bool("yes", false, "skip confirmation prompt")
	clusterDownCmd.Flags().Bool("purge", false, "also remove pmcluster-managed secrets, configs, and networks")

	clusterCmd.AddCommand(clusterUpCmd, clusterUpdateCmd, clusterStatusCmd, clusterDownCmd)
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

	// Refresh the on-disk config templates from this binary's embedded copies.
	// After a pmcluster binary upgrade, stale configs (older version header)
	// are overwritten so the re-render below picks up the new baked-in
	// templates (e.g. a newer edge-stack.yml) and re-deploys the stacks that
	// changed. Configs with the same or newer version header are preserved —
	// operator edits remain the source of truth.
	if err := cluster.EnsureConfigDir(cfg.ConfigDir(), buildinfo.Version); err != nil {
		return fmt.Errorf("refresh config dir: %w", err)
	}

	updateDomain := st.GetSettingDefault(ctx, "domain", "")
	res, err := cluster.Update(ctx, cluster.UpdateDeps{
		Store:       st,
		Cipher:      cipher,
		Docker:      dc,
		Deployer:    deployer,
		Stdout:      cmd.OutOrStdout(),
		Provisioner: ooProvisioner(st, cipher, cmd.OutOrStdout(), updateDomain),
	}, cluster.UpdateInput{
		ConfigDir: cfg.ConfigDir(),
		Version:   buildinfo.Version,
	})
	if err != nil {
		return err
	}
	printUpdateResult(cmd.OutOrStdout(), res)
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

func runClusterUp(cmd *cobra.Command, _ []string) error {
	defer initCLITelemetry()()

	ctx := cmd.Context()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return fmt.Errorf("data directory not initialised at %s — run `pmcluster init` first", cfg.DataDir)
	}

	// Console=false: cluster up uses fmt.Fprint for human progress; logger
	// only writes to the audit file here.
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

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

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

	res, err := cluster.Up(ctx, cluster.UpDeps{
		Store:       st,
		Cipher:      cipher,
		Docker:      dc,
		Deployer:    deployer,
		Stdout:      cmd.OutOrStdout(),
		Provisioner: ooProvisioner(st, cipher, cmd.OutOrStdout(), in.Domain),
	}, in)
	if err != nil {
		return err
	}

	printUpResult(cmd.OutOrStdout(), in, res)
	return nil
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
		order := []string{"traefik_dashboard", "portainer", "openobserve_admin"}
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
	fmt.Fprintf(out, "  Traefik    https://traefik.%s\n", in.Domain)
	fmt.Fprintf(out, "  Portainer  https://portainer.%s\n", in.Domain)
	fmt.Fprintf(out, "  OpenObserve https://observ.%s\n", in.Domain)
	fmt.Fprintf(out, "  pmcluster  https://pmcluster.%s   (after `pmcluster serve` is supervised)\n", in.Domain)
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

	report, err := cluster.Status(cmd.Context(), dc)
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
			ternary(purge, " AND purge pmcluster-managed secrets, configs, and overlay networks", ""),
		)
		return fmt.Errorf("not confirmed")
	}

	dc, err := docker.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = dc.Close() }()

	deployer := cluster.NewDockerCLIDeployer(cmd.OutOrStdout())
	res, err := cluster.Down(cmd.Context(), cluster.DownDeps{
		Docker:   dc,
		Deployer: deployer,
		Stdout:   cmd.OutOrStdout(),
	}, cluster.DownInput{Purge: purge})
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Removed:")
	fmt.Fprintf(out, "  Stacks   : %v\n", res.StacksRemoved)
	if purge {
		fmt.Fprintf(out, "  Secrets  : %v\n", res.SecretsRemoved)
		fmt.Fprintf(out, "  Configs  : %v\n", res.ConfigsRemoved)
		fmt.Fprintf(out, "  Networks : %v\n", res.NetworksRemoved)
	}
	return nil
}

func ternary[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}
