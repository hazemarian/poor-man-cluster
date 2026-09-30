package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// nodeNameRe mirrors the manifest placement-pin validation: a Docker node
// hostname or node ID — letters, digits, dots, dashes, underscores.
var nodeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

var joinCmd = &cobra.Command{
	Use:   "join",
	Short: "Join this host to the cluster's Docker Swarm",
	Long: `Joins this host to the cluster's Docker Swarm using a join token
obtained from an existing manager (docker swarm join-token worker|manager),
then initialises the local pmcluster state and starts the daemon.

The role (--role) selects the token type you need to pass: a worker token
joins as a worker, a manager token joins as a manager. After joining, pmcluster
verifies the node actually holds the requested role.

The daemon runs leader-aware: on the Swarm leader it serves; on every other
manager it stands by until Swarm elects it leader (failover). On a pure worker
node the daemon stays in standby.

Examples:
  pmcluster join --role worker  --token SWMTKN-1-... --manager 82.165.128.237:2377
  pmcluster join --role manager --token SWMTKN-1-... --manager 82.165.128.237:2377

After a successful join the local state is initialised (data dir, config,
database migrations) and the systemd unit is installed + started.

Quorum note for managers: keep an ODD count. 2 managers are worse than 1
(either loss still breaks quorum); 3 managers survive any single failure.`,
	RunE: runJoin,
}

func init() {
	joinCmd.Flags().String("token", "", "swarm join token (from 'docker swarm join-token worker|manager' on a manager)")
	joinCmd.Flags().String("manager", "", "manager advertise address, e.g. 10.0.0.5:2377")
	joinCmd.Flags().String("role", "worker", "role to join as: worker or manager (must match the token type)")
	joinCmd.Flags().String("hostname", "", "hostname this node joins the Swarm under (default: current OS hostname)")
	rootCmd.AddCommand(joinCmd)
}

func runJoin(cmd *cobra.Command, _ []string) error {
	token, _ := cmd.Flags().GetString("token")
	manager, _ := cmd.Flags().GetString("manager")
	role, _ := cmd.Flags().GetString("role")
	hostname, _ := cmd.Flags().GetString("hostname")
	switch role {
	case "worker", "manager":
	default:
		return fmt.Errorf("--role must be 'worker' or 'manager', got %q", role)
	}
	if token == "" {
		return fmt.Errorf("join requires --token (run 'docker swarm join-token worker|manager' on a manager)")
	}
	if manager == "" {
		return fmt.Errorf("join requires --manager (the manager advertise address, e.g. 10.0.0.5:2377)")
	}

	// Set the OS hostname before joining: the Swarm records the hostname at
	// join time, and the leader-aware daemon matches os.Hostname() against the
	// node list — so pinning (`placement: <hostname>`) only works reliably
	// when the join-time hostname is the one you intend.
	if hostname != "" {
		if err := setNodeHostname(cmd.Context(), cmd.OutOrStdout(), hostname); err != nil {
			return err
		}
	}

	// Run docker swarm join via the docker CLI (transparent to the operator).
	joinArgs := []string{"swarm", "join", "--token", token, manager}
	jc := exec.CommandContext(cmd.Context(), "docker", joinArgs...)
	jc.Stdout = cmd.OutOrStdout()
	jc.Stderr = cmd.ErrOrStderr()
	fmt.Fprintf(cmd.OutOrStdout(), "→ docker %s\n", strings.Join(joinArgs, " "))
	if err := jc.Run(); err != nil {
		return fmt.Errorf("docker swarm join failed: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "✔ joined the Swarm.")

	// Verify the node actually holds the requested role (the token type decides;
	// catch token/--role mismatches early).
	if err := verifyJoinedRole(cmd.Context(), cmd.OutOrStdout(), role); err != nil {
		return err
	}

	// Initialise local pmcluster state (idempotent — an existing data dir is
	// reused, never wiped).
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := prepareJoinState(cmd.Context(), cmd.OutOrStdout(), cfg); err != nil {
		return err
	}

	// Registry auth gap: a node without credentials for the app registry
	// silently serves stale cached images (Docker never pulls when the image
	// exists locally, and a private registry 401s any pull attempt). Surface
	// the gap at join time instead of leaving it to bite a later deploy.
	verifyRegistryAuth(cmd.OutOrStdout())

	// Install + start the daemon (systemd on Linux, hint otherwise).
	if err := ensureDaemonRunning(cmd.OutOrStdout()); err != nil {
		return err
	}

	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "✔ pmcluster join complete.")
	if role == "manager" {
		fmt.Fprintln(cmd.OutOrStdout(), "  This node is now a Swarm manager.")
		fmt.Fprintln(cmd.OutOrStdout(), "  The daemon is leader-aware: it serves when this node is the Swarm")
		fmt.Fprintln(cmd.OutOrStdout(), "  leader and stands by otherwise (failover).")
		fmt.Fprintln(cmd.OutOrStdout(), "  Quorum: keep the manager count ODD — 3 managers survive any single loss.")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "  This node is a Swarm worker.")
		fmt.Fprintln(cmd.OutOrStdout(), "  The daemon runs in standby on worker nodes. To serve here later,")
		fmt.Fprintln(cmd.OutOrStdout(), "  promote the node to manager:")
		fmt.Fprintln(cmd.OutOrStdout(), "    docker node promote <hostname>   # then it may be elected leader")
	}
	return nil
}

// setNodeHostname sets the OS hostname so the node joins the Swarm under the
// operator's chosen name (Swarm records the hostname at join time, and the
// leader-aware daemon matches os.Hostname() against the node list). It is a
// best-effort convenience: a failure to set the hostname is surfaced but is
// NOT fatal to the join itself — the node can join under its current name.
func setNodeHostname(ctx context.Context, out io.Writer, hostname string) error {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return nil
	}
	if !nodeNameRe.MatchString(hostname) {
		return fmt.Errorf("invalid hostname %q — letters, digits, dots, dashes and underscores only", hostname)
	}
	current, err := os.Hostname()
	if err == nil && current == hostname {
		fmt.Fprintf(out, "✔ hostname already %q.\n", hostname)
		return nil
	}

	// hostnamectl is the canonical Linux way; on other platforms (or when the
	// command is missing) surface a hint and continue — the join still works,
	// the node just keeps its current hostname.
	hc := exec.CommandContext(ctx, "hostnamectl", "set-hostname", hostname)
	if outB, err := hc.CombinedOutput(); err != nil {
		fmt.Fprintf(out, "⚠ could not set hostname to %q (%s) — joining under the current hostname.\n", hostname, strings.TrimSpace(string(outB)))
		return nil
	}
	fmt.Fprintf(out, "✔ hostname set to %q.\n", hostname)
	return nil
}

// verifyRegistryAuth warns when the local Docker config has no registry
// credentials. A node without credentials for a private app registry runs
// whatever image happens to be cached locally — silently stale — because
// Docker only pulls when the image is absent and private registries reject
// anonymous pulls. Best-effort: only a warning, the join itself succeeds.
func verifyRegistryAuth(out io.Writer) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	cfgPath := filepath.Join(home, ".docker", "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintln(out, "⚠ no docker registry credentials found (no ~/.docker/config.json).")
		fmt.Fprintln(out, "  Private registries (e.g. ghcr.io/your-org) will silently fall back to stale")
		fmt.Fprintln(out, "  cached images. Authenticate before deploying private images:")
		fmt.Fprintln(out, "    docker login ghcr.io --username <user>")
		return
	}
	var cfg struct {
		Auths map[string]any `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil || len(cfg.Auths) == 0 {
		fmt.Fprintln(out, "⚠ no docker registry credentials found (empty ~/.docker/config.json).")
		fmt.Fprintln(out, "  Private registries will silently fall back to stale cached images.")
		fmt.Fprintln(out, "    docker login ghcr.io --username <user>")
		return
	}
	fmt.Fprintln(out, "✔ docker registry credentials present (private pulls will stay fresh).")
}

// verifyJoinedRole checks the node's actual Swarm role after joining and
// reports a mismatch against the requested --role.
func verifyJoinedRole(ctx context.Context, out io.Writer, want string) error {
	ic := exec.CommandContext(ctx, "docker", "info", "--format", "{{.Swarm.Control}}")
	outB, err := ic.Output()
	if err != nil {
		// Not fatal — docker info may be noisy on some setups; the daemon is
		// leader-aware regardless of role.
		fmt.Fprintln(out, "⚠ could not verify the joined role (docker info unavailable).")
		return nil
	}
	isManager := strings.TrimSpace(string(outB)) == "true"
	switch {
	case want == "manager" && !isManager:
		return fmt.Errorf("requested --role manager but this node joined as a WORKER — you passed a worker token; run 'docker swarm join-token manager' on a manager and re-join")
	case want == "worker" && isManager:
		fmt.Fprintln(out, "⚠ joined as a MANAGER despite --role worker (the token was a manager token).")
	}
	return nil
}

// prepareJoinState creates the data dir + default config + runs DB migrations
// when they are missing, mirroring what `pmcluster init` does WITHOUT creating
// a bootstrap user (join nodes are not the control-plane owner; on promotion
// the leader-aware restore brings the control plane over).
func prepareJoinState(ctx context.Context, out io.Writer, cfg *config.Config) error {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	// Every container volume on this platform is forced under the volume root
	// (/var/stack/data) with a bind mount, and backup agents write tarballs to
	// /var/stack/backup. Both must exist on the joined node BEFORE the Swarm
	// scheduler can place tasks here — otherwise a task that lands on this
	// node is rejected at volume-populate ("no such file or directory") and
	// the service update pauses. Mirrors ensureStorageDirs in cluster up.
	const volumeRoot = "/var/stack/data"
	const backupRoot = "/var/stack/backup"
	for _, dir := range []string{volumeRoot, backupRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create storage dir %s: %w", dir, err)
		}
	}
	fmt.Fprintln(out, "✔ volume-root storage directories ready (/var/stack/data, /var/stack/backup).")
	if err := writeDefaultConfig(cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if _, err := os.Stat(cfg.DBPath()); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat db path: %w", err)
		}
		st, err := store.Open(cfg.DBPath())
		if err != nil {
			return fmt.Errorf("open store (migrations): %w", err)
		}
		_ = st.Close()
		fmt.Fprintln(out, "✔ local control-plane database initialised (migrations applied).")
	} else {
		fmt.Fprintln(out, "✔ local control-plane database already present (reused).")
	}
	return nil
}
