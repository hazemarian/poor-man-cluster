// Command pmcluster is the control-plane CLI and HTTP daemon for the
// poor-man-stack Docker Swarm cluster.
package main

import (
	"fmt"
	"os"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
