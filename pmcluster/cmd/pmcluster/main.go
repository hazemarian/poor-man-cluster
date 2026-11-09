// Command pmcluster is the control-plane CLI and HTTP daemon for the
// poor-man-cluster Docker Swarm cluster.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cli"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/logging"
)

func main() {
	// docker-credential-pmcluster is THIS binary under a symlink. Dispatch on
	// argv[0] before anything else runs: the helper's stdout must carry one JSON
	// object and nothing else, so no logging setup, no cobra and no log line may
	// have a chance to write there first.
	if filepath.Base(os.Args[0]) == credHelperName {
		os.Exit(runCredentialHelper())
	}

	// Configure the process log level before anything else: PMCLUSTER_LOG_LEVEL
	// is the knob the daemon shares with the edge container, and an unknown
	// value must degrade to info with one warning rather than abort the run
	// before a single line is written.
	logging.ConfigureFromEnv(logging.DefaultLevel)

	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
