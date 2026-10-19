// Package cli wires the Cobra command tree for the pmcluster binary.
package cli

import (
	"os"

	"github.com/spf13/cobra"
)

// configPath is set by the persistent --config flag and consumed by
// commands that load configuration (serve, cluster up, etc.).
var configPath string

// apiURL and apiToken enable remote mode: every command that has a service
// counterpart talks to the daemon REST API instead of the local store, so the
// CLI can run off the cluster node. Set via --api-url/--api-token flags or
// PMCLUSTER_API_URL / PMCLUSTER_API_TOKEN env vars.
var apiURL, apiToken string

// rootCmd is the top-level pmcluster command.
var rootCmd = &cobra.Command{
	Use:   "pmcluster",
	Short: "Control plane for the poor-man-cluster Docker Swarm",
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

// initVersion wires Cobra's built-in --version flag (auto-registered when
// Command.Version is non-empty; prints via the template below). The value is
// resolved at run time so ldflags/buildinfo stamps are honored.
func init() {
	rootCmd.Version = versionString()
	rootCmd.SetVersionTemplate(`pmcluster {{.Version}}
`)
	rootCmd.PersistentFlags().StringVar(
		&configPath,
		"config",
		"",
		`config file path (default: $HOME/.pmcluster/config.yaml)`,
	)
	rootCmd.PersistentFlags().StringVar(
		&apiURL,
		"api-url",
		"",
		"daemon API base URL (remote mode; default: $PMCLUSTER_API_URL)",
	)
	rootCmd.PersistentFlags().StringVar(
		&apiToken,
		"api-token",
		"",
		"daemon API bearer token (remote mode; default: $PMCLUSTER_API_TOKEN)",
	)

	// Env-var defaults for remote mode (documented in the README + deploy
	// skill). Flags take precedence over these when explicitly set.
	if v := os.Getenv("PMCLUSTER_API_URL"); v != "" && apiURL == "" {
		apiURL = v
	}
	if v := os.Getenv("PMCLUSTER_API_TOKEN"); v != "" && apiToken == "" {
		apiToken = v
	}

	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(clusterCmd)
}

// Execute runs the root command. main() exits non-zero on error.
func Execute() error {
	return rootCmd.Execute()
}
