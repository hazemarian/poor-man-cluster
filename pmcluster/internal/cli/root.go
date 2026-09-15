// Package cli wires the Cobra command tree for the pmcluster binary.
package cli

import (
	"github.com/spf13/cobra"
)

// configPath is set by the persistent --config flag and consumed by
// commands that load configuration (serve, cluster up, etc.).
var configPath string

// rootCmd is the top-level pmcluster command.
var rootCmd = &cobra.Command{
	Use:   "pmcluster",
	Short: "Control plane for the poor-man-stack Docker Swarm cluster",
	Long: `pmcluster is a single-binary control plane for a poor-man's Docker Swarm
cluster. It bootstraps the cluster (pmcluster cluster up), serves a REST/webhook
API for application deployments (pmcluster serve), and exposes a CLI for the
same operations (pmcluster deploy, pmcluster rollback, etc.).

Bootstrap flow:

  1. Operator: install Docker; docker swarm init --advertise-addr <ip>
  2. Operator: install pmcluster — curl -fsSL .../install.sh | bash
  3. Operator: pmcluster init             # local state + admin token
  4. Operator: pmcluster cluster up       # creates secrets/networks, deploys stacks
  5. Operator: pmcluster serve            # run the daemon (supervise via systemd)

After bootstrap, application deployments arrive via webhook, REST API, or
the pmcluster deploy CLI command.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().StringVar(
		&configPath,
		"config",
		"",
		`config file path (default: $HOME/.pmcluster/config.yaml)`,
	)

	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(clusterCmd)
}

// Execute runs the root command. main() exits non-zero on error.
func Execute() error {
	return rootCmd.Execute()
}
