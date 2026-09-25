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

The daemon runs leader-aware: on the Swarm leader it serves; on every other
manager it stands by until Swarm elects it leader (failover). On a pure worker
node the daemon stays in standby — promote the node to manager if you want it
to serve.

Example:
  pmcluster join --token SWMTKN-1-... --manager 82.165.128.237:2377

After a successful join the local state is initialised (data dir, config,
database migrations) and the systemd unit is installed + started.`,
	RunE: runJoin,
}

func init() {
	joinCmd.Flags().String("token", "", "swarm join token (from 'docker swarm join-token worker|manager' on a manager)")
	joinCmd.Flags().String("manager", "", "manager advertise address, e.g. 10.0.0.5:2377")
	rootCmd.AddCommand(joinCmd)
}

func runJoin(cmd *cobra.Command, _ []string) error {
	token, _ := cmd.Flags().GetString("token")
	manager, _ := cmd.Flags().GetString("manager")
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
	fmt.Fprintln(cmd.OutOrStdout(), "  The daemon is leader-aware: it serves on the Swarm leader and")
	fmt.Fprintln(cmd.OutOrStdout(), "  stands by on other managers. Promote this node to manager with:")
	fmt.Fprintln(cmd.OutOrStdout(), "    docker node promote <hostname>   # then it may be elected leader")
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
