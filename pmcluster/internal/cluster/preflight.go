// Package cluster owns the lifecycle of the bundled Docker Swarm cluster
// (Traefik, OpenObserve, OTel Collector, volume backup).
// All operations are idempotent — re-running cluster up reconciles state,
// never destroys or rotates.
package cluster

import (
	"context"
	"errors"
	"fmt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/runtime"
)

// PreflightError pairs a low-level cause with an operator-facing
// remediation hint suitable for direct printing.
type PreflightError struct {
	Cause       error
	Remediation string
}

func (e *PreflightError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%v\n\n%s", e.Cause, e.Remediation)
	}
	return e.Remediation
}

func (e *PreflightError) Unwrap() error { return e.Cause }

// Preflight checks (in order): docker reachable, swarm active, this
// node is a manager. Returns the first failure as *PreflightError.
func Preflight(ctx context.Context, d runtime.Client) error {
	if _, err := d.Ping(ctx); err != nil {
		return &PreflightError{
			Cause: err,
			Remediation: "Docker daemon is not reachable.\n" +
				"  • If Docker is not installed: https://docs.docker.com/engine/install/\n" +
				"  • If installed: start it (systemctl start docker, or open Docker Desktop)\n" +
				"  • Ensure your user can talk to the socket (group membership, DOCKER_HOST, etc.)",
		}
	}

	info, err := d.Info(ctx)
	if err != nil {
		return &PreflightError{
			Cause:       err,
			Remediation: "docker info failed; check daemon health.",
		}
	}

	if info.SwarmLocalNodeState != "active" {
		return &PreflightError{
			Cause: errors.New("Swarm not active on this node (state: " + info.SwarmLocalNodeState + ")"),
			Remediation: "Initialise Swarm before running pmcluster cluster up:\n" +
				"  docker swarm init --advertise-addr <this-node-ip>\n\n" +
				"pmcluster intentionally does NOT init Swarm — that decision " +
				"belongs to the operator.",
		}
	}

	if !info.SwarmControlAvailable {
		return &PreflightError{
			Cause: errors.New("this node is a Swarm worker, not a manager"),
			Remediation: "pmcluster cluster up must run on a Swarm manager.\n" +
				"  Workers should only run: docker swarm join --token <TOKEN> <MANAGER>:2377",
		}
	}

	return nil
}
