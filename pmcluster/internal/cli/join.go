package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

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
	rootCmd.AddCommand(joinCmd)
}

func runJoin(cmd *cobra.Command, _ []string) error {
	token, _ := cmd.Flags().GetString("token")
	manager, _ := cmd.Flags().GetString("manager")
	role, _ := cmd.Flags().GetString("role")
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
