// Command pmcluster is the control-plane CLI and HTTP daemon for the
// poor-man-cluster Docker Swarm cluster.
package main

import (
	"fmt"
	"os"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cli"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/logging"
)

func main() {
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
