package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/storage"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// setupCmd is an interactive wizard that collects the cluster configuration
// (domain, TLS mode, OpenObserve admin, SSO, edge login) into the local store
// settings and then hands off to `cluster up` (fresh install) or
// `cluster update` (existing cluster). Every answer is also available as a
// flag for non-interactive/scripted runs.
var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Interactive wizard: collect cluster config then run cluster up/update",
	Long: `Walks you through the cluster configuration questions and persists the
answers into the store settings:

  - Cluster domain (e.g. example.com)
  - TLS: Let's Encrypt (--acme-email) or operator-supplied cert/key (--cert/--key)
  - Traefik admin user
  - SSO: enable GitHub sign-in via an OAuth2 proxy (--sso-enabled,
    --sso-client-id, --sso-client-secret, --sso-github-org,
    --sso-github-repos, --sso-cookie-expire)
  - Edge console login (EDGE_LOGIN_DISABLED)

The OpenObserve admin email is derived automatically (admin@<domain>) — it is
hidden behind the admin-auth / SSO gate, so there is no prompt for it.

Every SSO question also has an env-var override for scripting/CI:
$PMCLUSTER_SSO_ENABLED, $PMCLUSTER_SSO_CLIENT_ID, $PMCLUSTER_SSO_CLIENT_SECRET,
$PMCLUSTER_SSO_GITHUB_ORG, $PMCLUSTER_SSO_COOKIE_EXPIRE, $PMCLUSTER_EDGE_LOGIN_ENABLED.

On a fresh install it then runs 'pmcluster cluster up'; on an existing
cluster it runs 'pmcluster cluster update'. Every question has a matching
flag so the wizard can be skipped entirely in scripts. SSO settings also
honor env-var overrides when their flags are absent: PMCLUSTER_SSO_ENABLED,
PMCLUSTER_SSO_CLIENT_ID, PMCLUSTER_SSO_CLIENT_SECRET, PMCLUSTER_SSO_GITHUB_ORG
and PMCLUSTER_EDGE_LOGIN_ENABLED (so the GitHub org can be changed from the
shell without touching the wizard).`,
	RunE: runSetup,
}

func init() {
	rootCmd.AddCommand(setupCmd)

	// Non-interactive flags mirror every prompt (see runSetup).
	setupCmd.Flags().String("domain", "", "cluster domain (e.g. example.com)")
	setupCmd.Flags().String("acme-email", "", "Let's Encrypt contact email (enables ACME)")
	setupCmd.Flags().String("cert", "", "operator-supplied TLS certificate PEM path")
	setupCmd.Flags().String("key", "", "operator-supplied TLS key PEM path")
	setupCmd.Flags().String("observability-admin-email", "", "observability admin email (default admin@<domain>)")
	setupCmd.Flags().String("traefik-admin-user", "", "Traefik dashboard admin user")
	setupCmd.Flags().Bool("sso-enabled", false, "enable GitHub SSO via OAuth2 proxy (default: $PMCLUSTER_SSO_ENABLED)")
	setupCmd.Flags().String("sso-client-id", "", "GitHub OAuth app client ID (SSO; default: $PMCLUSTER_SSO_CLIENT_ID)")
	setupCmd.Flags().String("sso-client-secret", "", "GitHub OAuth app client secret (SSO; default: $PMCLUSTER_SSO_CLIENT_SECRET)")
	setupCmd.Flags().String("sso-github-org", "", "restrict SSO to a GitHub org (optional; default: $PMCLUSTER_SSO_GITHUB_ORG)")
	setupCmd.Flags().String("sso-github-repos", "", "restrict SSO to users with access to these comma-separated repos, org/repo (optional; default: $PMCLUSTER_SSO_GITHUB_REPOS)")
	setupCmd.Flags().String("sso-cookie-expire", "", "SSO session cookie lifetime (default 1h; default: $PMCLUSTER_SSO_COOKIE_EXPIRE)")
	setupCmd.Flags().Bool("edge-login-enabled", false, "keep the edge console password login (default: disabled behind SSO/admin-auth; default: $PMCLUSTER_EDGE_LOGIN_ENABLED)")
	setupCmd.Flags().String("volume-root", "", "host dir every container volume is forced under (default /var/stack/data)")
	setupCmd.Flags().Bool("backup-enabled", true, "enable the backup pipeline (the agent asks the control plane before every run)")
	setupCmd.Flags().String("backup-retention-days", "", "how many days backups are kept before pruning (default 15)")
	setupCmd.Flags().String("oo-logs-retention-days", "", "Parseable log retention in days (default 30)")
	setupCmd.Flags().String("oo-metrics-retention-days", "", "Parseable metric retention in days (default 14)")
	setupCmd.Flags().String("oo-traces-retention-days", "", "Parseable trace retention in days (default 10)")
	setupCmd.Flags().String("backup-s3-endpoint", "", "offsite S3-compatible endpoint for backup uploads (R2/MinIO/AWS; e.g. https://<account>.r2.cloudflarestorage.com)")
	setupCmd.Flags().String("backup-s3-bucket", "", "offsite S3 bucket name for backup uploads")
	setupCmd.Flags().String("backup-s3-access-key", "", "offsite S3 access key (default: $PMCLUSTER_BACKUP_S3_ACCESS_KEY)")
	setupCmd.Flags().String("backup-s3-secret-key", "", "offsite S3 secret key (default: $PMCLUSTER_BACKUP_S3_SECRET_KEY)")
	setupCmd.Flags().String("backup-s3-region", "", "offsite S3 region (default auto for R2)")
	setupCmd.Flags().String("backup-store-on", "", "where the in-cluster backup store runs: leader or worker (default worker)")
	setupCmd.Flags().Bool("storage-failover", true, "enable automatic storage failover (moves stateful stacks off a failed storage node); requires offsite S3 backups")
	setupCmd.Flags().String("storage-mode", "loop", "how the snapshot-capable app-data volume is backed: loop (a file, no operator action needed), lvm (a block device) or none")
	setupCmd.Flags().String("storage-size", "auto", "backing-file size for the app-data volume (auto = half of free space, clamped to 8 GiB..128 GiB)")
	setupCmd.Flags().Bool("github-runners-enabled", false, "enable self-hosted GitHub Actions runners (requires a configured GitHub App)")
	setupCmd.Flags().String("github-runner-org", "", "GitHub organisation whose runner group the runner registers into (default: $PMCLUSTER_GITHUB_RUNNER_ORG)")
	setupCmd.Flags().String("github-runner-group", cluster.DefaultGitHubRunnerGroup, "org runner group the runner joins (created with visibility 'selected' when absent; never 'all')")
	setupCmd.Flags().String("github-runner-nodes", "", "comma-separated WORKER hostnames that may run a runner; managers are refused (default: $PMCLUSTER_GITHUB_RUNNER_NODES)")
	setupCmd.Flags().Bool("github-runner-socket-proxy", true, "route a runner's docker access through the daemon's filtered socket instead of /var/run/docker.sock")
	setupCmd.Flags().String("github-runner-memory", "", "memory limit per runner container (default 2560m)")
	setupCmd.Flags().Bool("github-runner-allow-leader", false, "allow runner containers on the leader (single-node clusters only; default: off the leader)")
	// The App itself is optional here: skipping it is a supported answer, and
	// the message printed when the flags are absent says where to finish later.
	setupCmd.Flags().String("github-app-id", "", "GitHub App id to store during setup (optional; can be done later from the console)")
	setupCmd.Flags().String("github-app-key-file", "", "path to the GitHub App private key .pem (optional; requires --github-app-id)")
	setupCmd.Flags().String("github-installation-id", "", "GitHub App installation id (optional; required with --github-app-id)")
	setupCmd.Flags().String("hostname", "", "hostname this node joins the Swarm under (default: current OS hostname)")

	setupCmd.Flags().String("swarm-advertise-addr", "", "advertise address passed to `docker swarm init` on a first node (default: auto-detected node IP)")
	setupCmd.Flags().Bool("tailscale", false, "first node: bring this node onto a private WireGuard tailnet (tailscale CLI required) and init the Swarm advertising the tailnet IPv4")
	setupCmd.Flags().String("tailscale-auth-key", "", "tailnet auth key for 'tailscale up' (default: $PMCLUSTER_TAILSCALE_AUTH_KEY)")
}

// setupAnswers is the collected wizard state.
type setupAnswers struct {
	Domain                  string
	ACMEEmail               string
	CertPath                string
	KeyPath                 string
	ObservabilityAdminEmail string
	TraefikAdminUser        string

	SSOEnabled      bool
	SSOProvider     string
	SSOClientID     string
	SSOClientSecret string
	SSOGitHubOrg    string
	SSOGitHubRepos  string
	SSOCookieExpire string

	EdgeLoginEnabled bool

	VolumeRoot string
	// BackupEnabled is the master switch. Asked first, because saying no skips
	// the store, the offsite bucket and the agent entirely.
	BackupEnabled bool

	BackupRetentionDays               string
	ObservabilityLogsRetentionDays    string
	ObservabilityMetricsRetentionDays string
	ObservabilityTracesRetentionDays  string
	BackupS3Endpoint                  string
	BackupS3Bucket                    string
	BackupS3AccessKey                 string
	BackupS3SecretKey                 string
	BackupS3Region                    string
	// BackupStoreOn is where the in-cluster backup store (SeaweedFS) runs:
	// "leader" or "worker". Empty means worker/auto (back-compat).
	BackupStoreOn string
	// StorageMode / StorageSize describe the app-data volume. Default loop:
	// a file on the root filesystem is the only option the platform can create
	// without an operator doing something it cannot verify.
	StorageMode string
	StorageSize string

	// StorageFailover enables automatic failover: when a storage node fails,
	// the control loop moves stateful stacks to a healthy storage node and
	// restores the latest offsite backup. Requires S3 offsite backups.
	StorageFailover bool

	// Self-hosted GitHub Actions runners. Off unless the operator turns it on,
	// and refused without a configured GitHub App — see
	// settings.normalizeRunnerSetting for why a PAT is not a substitute.
	GitHubRunnersEnabled    bool
	GitHubRunnerOrg         string
	GitHubRunnerGroup       string
	GitHubRunnerNodes       string
	GitHubRunnerSocketProxy bool
	GitHubRunnerMemory      string
	GitHubRunnerAllowLeader bool

	// The GitHub App itself, optional at setup time. All three empty means the
	// operator skipped it, and the console (or `pmcluster github app set`) can
	// do it later. Given, it goes through the same validate-by-minting path the
	// console uses, so setup cannot store a key that does not work.
	GitHubAppID          string
	GitHubAppKeyFile     string
	GitHubInstallationID string

	// NodeHostname is the name this node joins the Swarm under. It is applied
	// with hostnamectl before the cluster comes up so the Swarm records the
	// intended name (which the placement pins and the leader-aware daemon's
	// hostname match).
	NodeHostname string
	// SwarmAdvertiseAddr is passed to `docker swarm init` on a first node that
	// is not yet part of a Swarm. Empty lets Docker (or detectNodeIP) choose.
	SwarmAdvertiseAddr string
}

// ask prompts for a free-form value with a default; returns the trimmed answer.
func ask(r *bufio.Reader, out io.Writer, prompt, def string) string {
	label := prompt
	if def != "" {
		label = fmt.Sprintf("%s [%s]", prompt, def)
	}
	fmt.Fprintf(out, "%s: ", label)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// askYesNo prompts for a yes/no answer; returns def when the input is empty.
func askYesNo(r *bufio.Reader, out io.Writer, prompt string, def bool) bool {
	defLabel := "n"
	if def {
		defLabel = "y"
	}
	for {
		ans := strings.ToLower(ask(r, out, fmt.Sprintf("%s (y/n)", prompt), defLabel))
		switch ans {
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Fprintln(out, "  (answer y or n)")
	}
}

// askSecret prompts for a secret value with masked (no-echo) input and never
// echoes a persisted default back to the terminal (it shows "[set]" instead).
// Empty input reuses def, so an idempotent re-run keeps the stored secret
// without retyping it. Falls back to a plain line read when stdin is not a
// terminal.
func askSecret(r *bufio.Reader, out io.Writer, prompt, def string) string {
	label := prompt
	if def != "" {
		label = fmt.Sprintf("%s [set]", prompt)
	}
	fmt.Fprintf(out, "%s: ", label)

	var line string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(out)
		if err == nil {
			line = strings.TrimSpace(string(b))
		}
	} else {
		raw, _ := r.ReadString('\n')
		line = strings.TrimSpace(raw)
	}
	if line == "" {
		return def
	}
	return line
}

// askBackupStoreOn prompts where the in-cluster backup store should run:
// "leader" or "a worker" (default worker). Returns "leader" or "worker".
func askBackupStoreOn(r *bufio.Reader, out io.Writer, def string) string {
	if def != "leader" && def != "worker" {
		def = "worker"
	}
	for {
		line := strings.ToLower(ask(r, out, "Run the in-cluster backup store on 'leader' or 'a worker'?", def))
		switch line {
		case "leader":
			return "leader"
		case "worker", "a worker":
			return "worker"
		}
		fmt.Fprintln(out, "  (answer leader or worker)")
	}
}

func runSetup(cmd *cobra.Command, _ []string) error {
	defer initCLITelemetry()()

	ctx := cmd.Context()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return fmt.Errorf("data directory not initialised at %s — run `pmcluster init` first", cfg.DataDir)
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	out := cmd.OutOrStdout()
	interactive := !cmd.Flags().Changed("domain") && isTTY(cmd)

	var a setupAnswers
	if interactive {
		r := bufio.NewReader(os.Stdin)
		fmt.Fprintln(out)
		fmt.Fprintln(out, "┌──────────────────────────────────────────────┐")
		fmt.Fprintln(out, "│  pmcluster setup — cluster configuration      │")
		fmt.Fprintln(out, "└──────────────────────────────────────────────┘")

		a.Domain = ask(r, out, "Cluster domain", st.SettingDefault(ctx, cluster.SettingDomain(), ""))
		le := askYesNo(r, out, "Use Let's Encrypt?", a.ACMEEmail != "")
		if le {
			a.ACMEEmail = ask(r, out, "ACME contact email", st.SettingDefault(ctx, cluster.SettingTLSACME(), ""))
		} else {
			a.CertPath = ask(r, out, "TLS certificate PEM path", "")
			a.KeyPath = ask(r, out, "TLS key PEM path", "")
		}
		a.ObservabilityAdminEmail = defaultOOEmail(a.Domain)
		a.TraefikAdminUser = ask(r, out, "Traefik admin user", "admin")
		a.SSOEnabled = askYesNo(r, out, "Enable SSO (GitHub)?", st.SettingDefault(ctx, cluster.SettingSSOEnabled(), "") == "true")
		if a.SSOEnabled {
			a.SSOProvider = ask(r, out, "SSO provider", "github")
			a.SSOClientID = ask(r, out, "GitHub OAuth client ID", "")
			a.SSOClientSecret = ask(r, out, "GitHub OAuth client secret", "")
			a.SSOGitHubOrg = ask(r, out, "Restrict to GitHub org (optional)", st.SettingDefault(ctx, cluster.SettingSSOGitHubOrg(), ""))
			a.SSOGitHubRepos = ask(r, out, "Restrict to GitHub repos, comma-separated (optional)", st.SettingDefault(ctx, cluster.SettingSSOGitHubRepos(), ""))
			a.SSOCookieExpire = ask(r, out, "Session cookie lifetime (e.g. 1h)", st.SettingDefault(ctx, cluster.SettingSSOCookieExpire(), "1h"))
		}
		a.EdgeLoginEnabled = askYesNo(r, out, "Keep edge console password login?", false)
		// Runners are the CI answer to exhausted GitHub-hosted minutes. The
		// prompt states the three things that make it a decision rather than a
		// default: one container per selected WORKER, an org-level registration
		// scoped by a runner group, and the filtered docker socket.
		a.GitHubRunnersEnabled = askYesNo(r, out,
			"Enable self-hosted GitHub Actions runners? (one container per selected worker; skippable)",
			st.SettingDefault(ctx, cluster.SettingGitHubRunnersEnabled(), "") == "true")
		if a.GitHubRunnersEnabled {
			fmt.Fprintln(out, "  Runners register at ORG level with the GitHub App's installation token, so the App must")
			fmt.Fprintln(out, "  already be configured (`pmcluster github app set`) and hold Organization →")
			fmt.Fprintln(out, "  Self-hosted runners: Read & write (Organization permissions).")
			a.GitHubRunnerOrg = ask(r, out, "GitHub organisation for the runner",
				cluster.LoadGitHubRunnerOrg(ctx, st))
			a.GitHubRunnerGroup = ask(r, out, "Runner group (created with visibility 'selected')",
				cluster.LoadGitHubRunnerGroup(ctx, st))
			a.GitHubRunnerNodes = ask(r, out, "WORKER hostnames that may run a runner, comma-separated (managers are refused)",
				cluster.LoadGitHubRunnerNodes(ctx, st))
			a.GitHubRunnerSocketProxy = askYesNo(r, out,
				"Route the runner's docker access through the filtered socket? (say no only to debug a build)",
				cluster.LoadGitHubRunnerSocketProxy(ctx, st))
			a.GitHubRunnerMemory = ask(r, out, "Memory limit per runner",
				cluster.LoadGitHubRunnerMemory(ctx, st))
			a.GitHubRunnerAllowLeader = askYesNo(r, out,
				"Allow a runner container on the leader? (only on a single-node cluster)",
				st.SettingDefault(ctx, cluster.SettingGitHubRunnerAllowLeader(), "") == "true")
		}
		a.VolumeRoot = ask(r, out, "Container volume root dir", st.SettingDefault(ctx, cluster.SettingVolumeRoot(), manifest.DefaultVolumeRoot))
		a.BackupEnabled = askYesNo(r, out, "Enable backups? (snapshot every stack, hourly by default)", true)
		if !a.BackupEnabled {
			// No means no: nothing is installed and no store is deployed.
			a.BackupStoreOn = "off"
			fmt.Fprintln(out, "  Backups are OFF. Nothing is snapshotted, no backup store is deployed.")
			fmt.Fprintln(out, "  Handled elsewhere instead? Attach a block device to the host and back it up")
			fmt.Fprintln(out, "  with your own tooling, or point stacks at an external database service.")
			fmt.Fprintln(out, "  Turn platform backups on later: `pmcluster backup enable`, or the console.")
		} else {
			// The schedule lives in the platform's settings, not in the unit file.
			fmt.Fprintln(out, "  The platform decides WHEN to back up, from its own settings (console or CLI).")
			fmt.Fprintln(out, "  Do NOT edit, disable or re-time the pmcluster-backup timer: it is a heartbeat")
			fmt.Fprintln(out, "  that asks the control plane, so changing it does not change the schedule.")
			a.BackupRetentionDays = ask(r, out, "Backup retention (days)", st.SettingDefault(ctx, cluster.SettingBackupRetentionDays(), "3"))
			if askYesNo(r, out, "Keep every backup in one place in the cluster? (an in-cluster store)", true) {
				a.BackupStoreOn = askBackupStoreOn(r, out, st.SettingDefault(ctx, cluster.SettingBackupStoreOn(), ""))
			} else {
				a.BackupStoreOn = "off"
				fmt.Fprintln(out, "  Backups stay on the node that took them, and restores read them from there.")
				fmt.Fprintln(out, "  If that node is lost, its own backups are lost with it. Every run records")
				fmt.Fprintln(out, "  where each stack's artifact lives (node, store, offsite), so the console can")
				fmt.Fprintln(out, "  always answer \"where is the copy of this stack\".")
				if askYesNo(r, out, "Export backups offsite anyway (S3/R2)?", st.SettingDefault(ctx, cluster.SettingBackupS3Endpoint(), "") != "") {
					a.BackupS3Endpoint = ask(r, out, "S3-compatible endpoint (R2: https://<account>.r2.cloudflarestorage.com)", st.SettingDefault(ctx, cluster.SettingBackupS3Endpoint(), ""))
					a.BackupS3Bucket = ask(r, out, "S3 bucket name", st.SettingDefault(ctx, cluster.SettingBackupS3Bucket(), ""))
					a.BackupS3Region = ask(r, out, "S3 region", st.SettingDefault(ctx, cluster.SettingBackupS3Region(), "auto"))
					a.BackupS3AccessKey = askSecret(r, out, "S3 access key", st.SettingDefault(ctx, cluster.SettingBackupS3AccessKey(), ""))
					a.BackupS3SecretKey = askSecret(r, out, "S3 secret key", st.SettingDefault(ctx, cluster.SettingBackupS3SecretKey(), ""))
				}
			}
		}
		defHost, _ := os.Hostname()
		a.NodeHostname = ask(r, out, "Hostname for this node (pins use placement: <hostname>)", defHost)
		if !swarmActive(ctx) {
			defAdvertise := detectNodeIP()
			advertise := ask(r, out, "Initialise Swarm on this node? advertise address (first node)", defAdvertise)
			a.SwarmAdvertiseAddr = advertise
		}
	} else {
		a.Domain, _ = cmd.Flags().GetString("domain")
		a.ACMEEmail, _ = cmd.Flags().GetString("acme-email")
		a.CertPath, _ = cmd.Flags().GetString("cert")
		a.KeyPath, _ = cmd.Flags().GetString("key")
		a.ObservabilityAdminEmail, _ = cmd.Flags().GetString("observability-admin-email")
		if a.ObservabilityAdminEmail == "" {
			// Hidden behind the admin-auth/SSO gate — no need to prompt.
			a.ObservabilityAdminEmail = defaultOOEmail(a.Domain)
		}
		a.TraefikAdminUser, _ = cmd.Flags().GetString("traefik-admin-user")
		a.SSOEnabled, _ = cmd.Flags().GetBool("sso-enabled")
		a.SSOClientID, _ = cmd.Flags().GetString("sso-client-id")
		a.SSOClientSecret, _ = cmd.Flags().GetString("sso-client-secret")
		a.SSOGitHubOrg, _ = cmd.Flags().GetString("sso-github-org")
		a.SSOGitHubRepos, _ = cmd.Flags().GetString("sso-github-repos")
		a.SSOCookieExpire, _ = cmd.Flags().GetString("sso-cookie-expire")
		a.EdgeLoginEnabled, _ = cmd.Flags().GetBool("edge-login-enabled")
		a.VolumeRoot, _ = cmd.Flags().GetString("volume-root")
		a.BackupRetentionDays, _ = cmd.Flags().GetString("backup-retention-days")
		a.ObservabilityLogsRetentionDays, _ = cmd.Flags().GetString("oo-logs-retention-days")
		a.ObservabilityMetricsRetentionDays, _ = cmd.Flags().GetString("oo-metrics-retention-days")
		a.ObservabilityTracesRetentionDays, _ = cmd.Flags().GetString("oo-traces-retention-days")
		a.BackupS3Endpoint, _ = cmd.Flags().GetString("backup-s3-endpoint")
		a.BackupS3Bucket, _ = cmd.Flags().GetString("backup-s3-bucket")
		a.BackupEnabled, _ = cmd.Flags().GetBool("backup-enabled")
		a.BackupS3AccessKey, _ = cmd.Flags().GetString("backup-s3-access-key")
		a.BackupS3SecretKey, _ = cmd.Flags().GetString("backup-s3-secret-key")
		a.BackupS3Region, _ = cmd.Flags().GetString("backup-s3-region")
		a.BackupStoreOn, _ = cmd.Flags().GetString("backup-store-on")
		// Automatic storage failover recovers from offsite backups, so it stays
		// off unless S3 is configured (even if --storage-failover was passed).
		a.StorageFailover, _ = cmd.Flags().GetBool("storage-failover")
		a.StorageFailover = storageFailoverEnabled(a.StorageFailover, a.BackupS3Endpoint, a.BackupS3Bucket)
		a.StorageMode, _ = cmd.Flags().GetString("storage-mode")
		a.StorageSize, _ = cmd.Flags().GetString("storage-size")
		a.GitHubRunnersEnabled, _ = cmd.Flags().GetBool("github-runners-enabled")
		a.GitHubRunnerOrg, _ = cmd.Flags().GetString("github-runner-org")
		a.GitHubRunnerGroup, _ = cmd.Flags().GetString("github-runner-group")
		a.GitHubRunnerNodes, _ = cmd.Flags().GetString("github-runner-nodes")
		a.GitHubRunnerSocketProxy, _ = cmd.Flags().GetBool("github-runner-socket-proxy")
		a.GitHubRunnerMemory, _ = cmd.Flags().GetString("github-runner-memory")
		a.GitHubRunnerAllowLeader, _ = cmd.Flags().GetBool("github-runner-allow-leader")
		// Optional GitHub App registration; empty strings mean "skipped".
		a.GitHubAppID, _ = cmd.Flags().GetString("github-app-id")
		a.GitHubAppKeyFile, _ = cmd.Flags().GetString("github-app-key-file")
		a.GitHubInstallationID, _ = cmd.Flags().GetString("github-installation-id")
		if !cmd.Flags().Changed("github-runners-enabled") {
			if v := os.Getenv("PMCLUSTER_GITHUB_RUNNERS_ENABLED"); v != "" {
				a.GitHubRunnersEnabled = envBool(v)
			}
		}
		if !cmd.Flags().Changed("github-runner-org") {
			if v := os.Getenv("PMCLUSTER_GITHUB_RUNNER_ORG"); v != "" {
				a.GitHubRunnerOrg = v
			}
		}
		if !cmd.Flags().Changed("github-runner-nodes") {
			if v := os.Getenv("PMCLUSTER_GITHUB_RUNNER_NODES"); v != "" {
				a.GitHubRunnerNodes = v
			}
		}
		if !cmd.Flags().Changed("github-runner-socket-proxy") {
			if v := os.Getenv("PMCLUSTER_GITHUB_RUNNER_SOCKET_PROXY"); v != "" {
				a.GitHubRunnerSocketProxy = envBool(v)
			}
		}
		if v := os.Getenv("PMCLUSTER_STORAGE_MODE"); v != "" && !cmd.Flags().Changed("storage-mode") {
			a.StorageMode = v
		}
		if !cmd.Flags().Changed("backup-s3-access-key") {
			if v := os.Getenv("PMCLUSTER_BACKUP_S3_ACCESS_KEY"); v != "" {
				a.BackupS3AccessKey = v
			}
		}
		if !cmd.Flags().Changed("backup-s3-secret-key") {
			if v := os.Getenv("PMCLUSTER_BACKUP_S3_SECRET_KEY"); v != "" {
				a.BackupS3SecretKey = v
			}
		}
		a.NodeHostname, _ = cmd.Flags().GetString("hostname")
		if a.NodeHostname == "" {
			a.NodeHostname, _ = os.Hostname()
		}
		a.SwarmAdvertiseAddr, _ = cmd.Flags().GetString("swarm-advertise-addr")

		// M3 — optional tailnet for a first node: when the operator opts in and
		// did not already pin --swarm-advertise-addr, bring this node onto the
		// tailnet and let `docker swarm init` advertise the tailnet IPv4.
		if ts, _ := cmd.Flags().GetBool("tailscale"); ts && a.SwarmAdvertiseAddr == "" {
			authKey, _ := cmd.Flags().GetString("tailscale-auth-key")
			if authKey == "" {
				authKey = tailscaleAuthKeyFromEnv()
			}
			advertise, err := tailscaleReady(cmd.Context(), cmd.OutOrStdout(), authKey, a.NodeHostname)
			if err != nil {
				return err
			}
			a.SwarmAdvertiseAddr = advertise
		}

		// Env-var overrides for SSO settings so they can be changed from the
		// shell / CI without editing the wizard (same precedence model as
		// PMCLUSTER_API_URL in root.go): explicit flags win, then env vars,
		// then persisted settings.
		if !cmd.Flags().Changed("sso-enabled") {
			if v := os.Getenv("PMCLUSTER_SSO_ENABLED"); v != "" {
				a.SSOEnabled = envBool(v)
			}
		}
		if !cmd.Flags().Changed("sso-client-id") {
			if v := os.Getenv("PMCLUSTER_SSO_CLIENT_ID"); v != "" {
				a.SSOClientID = v
			}
		}
		if !cmd.Flags().Changed("sso-client-secret") {
			if v := os.Getenv("PMCLUSTER_SSO_CLIENT_SECRET"); v != "" {
				a.SSOClientSecret = v
			}
		}
		if !cmd.Flags().Changed("sso-github-org") {
			if v := os.Getenv("PMCLUSTER_SSO_GITHUB_ORG"); v != "" {
				a.SSOGitHubOrg = v
			}
		}
		if !cmd.Flags().Changed("sso-github-repos") {
			if v := os.Getenv("PMCLUSTER_SSO_GITHUB_REPOS"); v != "" {
				a.SSOGitHubRepos = v
			}
		}
		if !cmd.Flags().Changed("sso-cookie-expire") {
			if v := os.Getenv("PMCLUSTER_SSO_COOKIE_EXPIRE"); v != "" {
				a.SSOCookieExpire = v
			}
		}
		if a.SSOCookieExpire == "" {
			// Default to the tightened 1h session (oauth2-proxy's own
			// default is 168h). Kept as the stored fallback so unset SSO
			// deployments converge to the shorter lifetime.
			a.SSOCookieExpire = "1h"
		}
		if !cmd.Flags().Changed("edge-login-enabled") {
			if v := os.Getenv("PMCLUSTER_EDGE_LOGIN_ENABLED"); v != "" {
				a.EdgeLoginEnabled = envBool(v)
			}
		}
		if a.SSOEnabled {
			a.SSOProvider = "github"
		}
	}

	// Persist the collected answers into the store settings.
	if a.Domain == "" {
		return fmt.Errorf("domain is required (interactive prompt or --domain)")
	}
	if a.SSOEnabled && (a.SSOClientID == "" || a.SSOClientSecret == "") {
		return fmt.Errorf("SSO enabled but client ID/secret missing")
	}
	if a.ACMEEmail == "" && (a.CertPath == "" || a.KeyPath == "") {
		return fmt.Errorf("TLS mode required: either --acme-email or --cert/--key")
	}
	if err := validateRunnerSetup(ctx, st, a, out); err != nil {
		return err
	}

	// Optional GitHub App registration. It runs the same validate-by-minting path
	// the console's set/rotate form uses, so setup cannot leave a key behind that
	// does not work; the flags are never required.
	if err := configureGitHubAppFromSetup(ctx, st, cfg, a, out); err != nil {
		return err
	}

	// Apply the operator's chosen hostname before the cluster comes up. The
	// Swarm records the hostname at join/init time and the leader-aware
	// daemon matches os.Hostname() against the node list, so getting the name
	// right up front keeps `placement: <hostname>` pins valid. Best-effort:
	// if hostnamectl is unavailable the setup continues under the current
	// hostname.
	if a.NodeHostname != "" {
		if err := setNodeHostname(ctx, out, a.NodeHostname); err != nil {
			return err
		}
	}

	// The installed check must run BEFORE persisting — the wizard's own
	// settings would otherwise make a fresh install look like an existing
	// cluster.
	installed := cluster.ClusterInstalled(ctx, st)

	if installed {
		// Existing cluster: persist everything, then reconcile.
		if err := persistSetup(ctx, st, a); err != nil {
			return err
		}
		if spec, err := storageSpecFromSetup(a); err == nil && spec.Mode != storage.ModeNone {
			provisionStorageForNode(ctx, out, spec)
		}
		fmt.Fprintln(out, "Cluster already initialised — running `pmcluster cluster update` with the new settings.")
		return runClusterUpdate(cmd, nil)
	}

	// Fresh install: persist ONLY the settings Up's render step reads from the
	// store (SSO + edge login). Domain / OO email / TLS state must NOT be
	// persisted here — Up's own clusterInstalled guard would then reject the
	// install — so they are handed over through UpInput and Up persists them
	// itself via persistInstallState.
	if err := persistSetupSecretsOnly(ctx, st, a); err != nil {
		return err
	}

	// The volume must exist BEFORE the cluster deploys: cluster up can only
	// create directories, and a directory on the root filesystem cannot be
	// snapshotted — which is precisely the "backup while writing" failure this
	// platform is meant to remove.
	if spec, err := storageSpecFromSetup(a); err == nil {
		provisionStorageForNode(ctx, out, spec)
	} else {
		fmt.Fprintf(out, "⚠ app-data volume not provisioned: %v\n", err)
	}
	// Swap is the host's business, not the cluster's: harden-host.sh sizes it
	// from RAM and free disk when the node image is built.

	in := cluster.UpInput{}
	in.Version = buildinfo.Version
	in.Domain = a.Domain
	in.ACMEEmail = a.ACMEEmail
	in.CertPath = a.CertPath
	in.KeyPath = a.KeyPath
	in.ObservabilityAdminEmail = a.ObservabilityAdminEmail
	in.TraefikAdminUser = a.TraefikAdminUser
	in.ConfigDir = cfg.ConfigDir()
	in.SwarmAdvertiseAddr = a.SwarmAdvertiseAddr

	fmt.Fprintln(out, "Fresh install — running `pmcluster cluster up` with the new settings.")
	return runUp(cmd, cfg, in)
}

// configureGitHubAppFromSetup stores the App registration when the optional
// flags were passed, and says where to finish when they were not.
//
// Validation is githubapp.Local.Apply's: parse the PEM, sign a JWT, mint an
// installation token for the installation, probe, and only then persist. Setup
// therefore cannot leave a key that mints nothing behind, and skipping the whole
// thing is a supported answer — a cluster on public images needs no App.
func configureGitHubAppFromSetup(ctx context.Context, st *store.Store, cfg *config.Config, a setupAnswers, out io.Writer) error {
	if a.GitHubAppID == "" && a.GitHubAppKeyFile == "" && a.GitHubInstallationID == "" {
		fmt.Fprintln(out, "GitHub App: skipped. It can be configured later from the console")
		fmt.Fprintln(out, "  (Settings → GitHub App → \"Set or rotate the App key\"), or with `pmcluster github app set`.")
		return nil
	}
	appID, err := parseSetupID("--github-app-id", a.GitHubAppID)
	if err != nil {
		return err
	}
	installationID, err := parseSetupID("--github-installation-id", a.GitHubInstallationID)
	if err != nil {
		return err
	}
	if appID <= 0 {
		return errors.New("--github-app-id is required when any GitHub App flag is given")
	}
	if installationID <= 0 {
		return errors.New("--github-installation-id is required with --github-app-id: the registration is validated by minting an installation token for it, and without one there is nothing to prove")
	}
	if a.GitHubAppKeyFile == "" {
		return errors.New("--github-app-key-file is required with --github-app-id")
	}
	keyPEM, err := os.ReadFile(a.GitHubAppKeyFile)
	if err != nil {
		return fmt.Errorf("read --github-app-key-file: %w", err)
	}
	if len(keyPEM) > maxSetupKeyBytes {
		return fmt.Errorf("--github-app-key-file is %d bytes, larger than %d KiB, which is not a PEM", len(keyPEM), maxSetupKeyBytes>>10)
	}

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		return fmt.Errorf("open encryption key: %w", err)
	}
	l := &githubapp.Local{Store: st, Cipher: cipher, Client: &githubapp.Client{}}
	res, err := l.Apply(ctx, githubapp.SetRequest{
		AppID:          appID,
		InstallationID: installationID,
		PrivateKeyPEM:  keyPEM,
	})
	if err != nil {
		return fmt.Errorf("github app: %w\n  Fix the ids/key and re-run, or drop these flags and configure the App later from the console", err)
	}

	fmt.Fprintf(out, "GitHub App %d stored (installation %d), key %s.\n", res.AppID, res.InstallationID, res.KeyFingerprint)
	if res.Probe.OK {
		fmt.Fprintf(out, "  Probe: %s\n", res.Probe.Detail)
	} else {
		fmt.Fprintf(out, "  Probe: %s\n", res.Probe.Detail)
		fmt.Fprintln(out, "  Add --probe-repo / --probe-package later (console or `pmcluster github app set`) so doctor can prove read access.")
	}
	fmt.Fprintln(out, "  The private key is encrypted in data.db and is never displayed again.")
	return nil
}

// maxSetupKeyBytes is the ceiling for a key read from --github-app-key-file: a
// 4096-bit RSA PEM is under 4 KiB, so anything past this is not a key.
const maxSetupKeyBytes = 64 << 10

// parseSetupID reads a numeric setup flag. Empty is 0 (absent); anything
// unparseable is named rather than silently ignored.
func parseSetupID(flag, raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q", flag, raw)
	}
	return v, nil
}

// validateRunnerSetup enforces the runner feature's dependency at setup time,
// the same way the settings surface does: a runner registers with a GitHub App
// installation token, and a personal access token is not accepted instead
// (it would need administration:write on every repo). Erring here rather than
// persisting an enabled-but-inert flag means the wizard cannot leave the
// operator with a feature that will silently never register anything.
func validateRunnerSetup(ctx context.Context, st *store.Store, a setupAnswers, out io.Writer) error {
	if !a.GitHubRunnersEnabled {
		return nil
	}
	if _, err := st.GitHubApp(ctx); err != nil {
		return fmt.Errorf("self-hosted runners require a configured GitHub App: they register with an App installation token (`pmcluster github app set --app-id … --key-file …`). A personal access token is not an acceptable substitute — it would need administration:write on every repository, a long-lived credential with a far wider blast radius than the hour-long App token")
	}
	if _, err := runners.NormalizeOrg(a.GitHubRunnerOrg); err != nil {
		return fmt.Errorf("github-runner-org: %w", err)
	}
	if _, err := runners.NormalizeGroup(a.GitHubRunnerGroup); err != nil {
		return fmt.Errorf("github-runner-group: %w", err)
	}
	if _, err := runners.NormalizeNodes(a.GitHubRunnerNodes); err != nil {
		return fmt.Errorf("github-runner-nodes: %w", err)
	}
	if a.GitHubRunnerMemory != "" {
		if err := runners.ValidateMemory(a.GitHubRunnerMemory); err != nil {
			return fmt.Errorf("github-runner-memory: %w", err)
		}
	}
	return nil
}

// storageSpecFromSetup turns the wizard's answers into a provisioning spec.
func storageSpecFromSetup(a setupAnswers) (storage.Spec, error) {
	spec := storage.Spec{Mode: storage.Mode(a.StorageMode)}
	if spec.Mode == "" {
		spec.Mode = storage.ModeLoop
	}
	if a.StorageSize != "" && a.StorageSize != "auto" {
		n, err := parseSize(a.StorageSize)
		if err != nil {
			return spec, err
		}
		spec.Size = n
	}
	return spec, nil
}

// defaultOOEmail derives the OpenObserve root admin email from the cluster
// domain (admin@<domain>). The email is hidden behind the admin-auth / SSO
// gate and only used for the OO root user, so it is not worth prompting for.
func defaultOOEmail(domain string) string {
	if domain == "" {
		return ""
	}
	return "admin@" + domain
}

// envBool parses a boolean env var the same way the edge config does
// (1/true/yes/on are true).
func envBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// persistSetupSecretsOnly persists the settings the Up/Update render pipeline
// reads from the store (SSO + edge login). Domain / OO email / TLS state are
// intentionally excluded: on a fresh install Up receives them through
// UpInput and persists them itself.
func persistSetupSecretsOnly(ctx context.Context, st *store.Store, a setupAnswers) error {
	setting := map[string]string{
		cluster.SettingSSOEnabled():                        boolSetting(a.SSOEnabled),
		cluster.SettingSSOProvider():                       a.SSOProvider,
		cluster.SettingSSOClientID():                       a.SSOClientID,
		cluster.SettingSSOClientSecret():                   a.SSOClientSecret,
		cluster.SettingSSOGitHubOrg():                      a.SSOGitHubOrg,
		cluster.SettingSSOGitHubRepos():                    a.SSOGitHubRepos,
		cluster.SettingSSOCookieExpire():                   a.SSOCookieExpire,
		cluster.SettingEdgeLoginDisabled():                 boolSetting(!a.EdgeLoginEnabled),
		cluster.SettingVolumeRoot():                        a.VolumeRoot,
		cluster.SettingBackupEnabled():                     boolSetting(a.BackupEnabled),
		cluster.SettingBackupRetentionDays():               a.BackupRetentionDays,
		cluster.SettingObservabilityLogsRetentionDays():    a.ObservabilityLogsRetentionDays,
		cluster.SettingObservabilityMetricsRetentionDays(): a.ObservabilityMetricsRetentionDays,
		cluster.SettingObservabilityTracesRetentionDays():  a.ObservabilityTracesRetentionDays,
		cluster.SettingBackupS3Endpoint():                  a.BackupS3Endpoint,
		cluster.SettingBackupS3Bucket():                    a.BackupS3Bucket,
		cluster.SettingBackupS3AccessKey():                 a.BackupS3AccessKey,
		cluster.SettingBackupS3SecretKey():                 a.BackupS3SecretKey,
		cluster.SettingBackupS3Region():                    a.BackupS3Region,
		cluster.SettingBackupStoreOn():                     a.BackupStoreOn,
		cluster.SettingStorageFailover():                   boolSetting(a.StorageFailover),
	}
	for k, v := range runnerSettings(a) {
		setting[k] = v
	}
	for k, v := range setting {
		if err := st.SetSetting(ctx, k, v); err != nil {
			return fmt.Errorf("save setting %s: %w", k, err)
		}
	}
	return nil
}

// persistSetup writes every collected answer into the store settings.
func persistSetup(ctx context.Context, st *store.Store, a setupAnswers) error {
	setting := map[string]string{
		cluster.SettingDomain():                            a.Domain,
		cluster.SettingOOEmail():                           a.ObservabilityAdminEmail,
		cluster.SettingTraefikAdminUser():                  a.TraefikAdminUser,
		cluster.SettingSSOEnabled():                        boolSetting(a.SSOEnabled),
		cluster.SettingSSOProvider():                       a.SSOProvider,
		cluster.SettingSSOClientID():                       a.SSOClientID,
		cluster.SettingSSOClientSecret():                   a.SSOClientSecret,
		cluster.SettingSSOGitHubOrg():                      a.SSOGitHubOrg,
		cluster.SettingSSOGitHubRepos():                    a.SSOGitHubRepos,
		cluster.SettingSSOCookieExpire():                   a.SSOCookieExpire,
		cluster.SettingEdgeLoginDisabled():                 boolSetting(!a.EdgeLoginEnabled),
		cluster.SettingVolumeRoot():                        a.VolumeRoot,
		cluster.SettingBackupEnabled():                     boolSetting(a.BackupEnabled),
		cluster.SettingBackupRetentionDays():               a.BackupRetentionDays,
		cluster.SettingObservabilityLogsRetentionDays():    a.ObservabilityLogsRetentionDays,
		cluster.SettingObservabilityMetricsRetentionDays(): a.ObservabilityMetricsRetentionDays,
		cluster.SettingObservabilityTracesRetentionDays():  a.ObservabilityTracesRetentionDays,
		cluster.SettingBackupS3Endpoint():                  a.BackupS3Endpoint,
		cluster.SettingBackupS3Bucket():                    a.BackupS3Bucket,
		cluster.SettingBackupS3AccessKey():                 a.BackupS3AccessKey,
		cluster.SettingBackupS3SecretKey():                 a.BackupS3SecretKey,
		cluster.SettingBackupS3Region():                    a.BackupS3Region,
		cluster.SettingBackupStoreOn():                     a.BackupStoreOn,
		cluster.SettingStorageFailover():                   boolSetting(a.StorageFailover),
	}
	for k, v := range runnerSettings(a) {
		setting[k] = v
	}
	for k, v := range setting {
		if err := st.SetSetting(ctx, k, v); err != nil {
			return fmt.Errorf("save setting %s: %w", k, err)
		}
	}

	// TLS state: ACME or operator cert/key.
	tls := cluster.TLSState{
		Mode:      "operator",
		CertPath:  a.CertPath,
		KeyPath:   a.KeyPath,
		ACMEEmail: a.ACMEEmail,
	}
	if a.ACMEEmail != "" {
		tls.Mode = "acme"
	}
	if err := tls.Save(ctx, st); err != nil {
		return fmt.Errorf("save TLS state: %w", err)
	}
	return nil
}

func boolSetting(b bool) string {
	if b {
		return "true"
	}
	return ""
}

// runnerSettings renders the wizard's runner answers into the four settings
// rows, with the repo list normalised (trimmed, deduped, order preserved).
// Renderer errors are impossible here — validateRunnerSetup already rejected
// anything malformed — so a bad list would only be reached by a caller that
// skipped validation, and then persisting the raw value is no worse than
// dropping it.
func runnerSettings(a setupAnswers) map[string]string {
	org, _ := runners.NormalizeOrg(a.GitHubRunnerOrg)
	group, _ := runners.NormalizeGroup(a.GitHubRunnerGroup)
	nodes, _ := runners.NormalizeNodes(a.GitHubRunnerNodes)
	return map[string]string{
		cluster.SettingGitHubRunnersEnabled():    boolSetting(a.GitHubRunnersEnabled),
		cluster.SettingGitHubRunnerOrg():         org,
		cluster.SettingGitHubRunnerGroup():       group,
		cluster.SettingGitHubRunnerNodes():       nodes,
		cluster.SettingGitHubRunnerSocketProxy(): strconv.FormatBool(a.GitHubRunnerSocketProxy),
		cluster.SettingGitHubRunnerMemory():      strings.TrimSpace(a.GitHubRunnerMemory),
		cluster.SettingGitHubRunnerAllowLeader(): boolSetting(a.GitHubRunnerAllowLeader),
	}
}

// storageFailoverEnabled gates automatic storage failover on a configured
// offsite S3 destination: when a storage node dies the only recoverable copy
// lives in S3, so failover must refuse to enable without it.
func storageFailoverEnabled(requested bool, s3Endpoint, s3Bucket string) bool {
	return requested && s3Endpoint != "" && s3Bucket != ""
}

// isTTY reports whether the command's stdout is a terminal (interactive).
func isTTY(cmd *cobra.Command) bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
