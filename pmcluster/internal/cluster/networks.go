package cluster

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// pmclusterLabel is set on every Docker resource pmcluster creates so we
// can identify them later (e.g. for `cluster down --purge`).
const pmclusterLabel = "io.pmcluster.managed"

// PmclusterLabel is exported for cross-package access (serve health checks).
const PmclusterLabel = pmclusterLabel

// EnsureNetwork creates an attachable overlay network if it doesn't already
// exist. Returns true if the network was newly created (handy for logs).
//
// Idempotent: repeated calls are safe and cheap (one inspect, no recreate).
// Existing networks are NEVER reconfigured — if an operator pre-created the
// network with different settings, pmcluster respects their choice.
func EnsureNetwork(ctx context.Context, d runtime.Client, name string) (created bool, err error) {
	exists, err := d.NetworkExists(ctx, name)
	if err != nil {
		return false, fmt.Errorf("check network %s: %w", name, err)
	}
	if exists {
		return false, nil
	}
	err = d.NetworkCreate(ctx, runtime.NetworkSpec{
		Name:       name,
		Driver:     "overlay",
		Attachable: true,
	})
	if err != nil {
		return false, fmt.Errorf("create network %s: %w", name, err)
	}
	return true, nil
}

// EnsureBundledNetworks creates the two overlay networks the bundled stacks
// need: traefik-net (ingress) and monitoring-net (telemetry).
// Returns the names of any newly created networks.
func EnsureBundledNetworks(ctx context.Context, d runtime.Client) ([]string, error) {
	var created []string
	for _, name := range []string{"traefik-net", "monitoring-net"} {
		isNew, err := EnsureNetwork(ctx, d, name)
		if err != nil {
			return created, err
		}
		if isNew {
			created = append(created, name)
		}
	}
	return created, nil
}

// stackNetGoneTimeout bounds how long ensureStackNetwork waits for a
// superseded stack overlay network to finish being removed before giving up.
// Swarm removes overlay networks asynchronously, so a leftover can linger in a
// "removing" state for a few seconds.
const stackNetGoneTimeout = 15 * time.Second

// ensureStackOverlays creates every non-external (stack-private) overlay
// network a rendered stack declares, BEFORE that stack is deployed, and
// returns the names it newly created.
//
// WHY THIS EXISTS — the ordering gap it closes:
//
// `docker stack deploy` does not reference a compose-declared network by ID.
// The CLI scopes each compose network key with the stack name
// (docker/cli convertServiceNetworks → namespace.Scope(key), i.e. the private
// key "net" becomes "<stack>_net") and attaches every service to it BY NAME.
// It creates a network only when a label-scoped list — networks carrying
// com.docker.stack.namespace=<stack> — does NOT already contain that name. So
// the service create depends on the network being present in the manager under
// exactly that name at that instant, and it is created in the same
// `docker stack deploy` call that immediately references it.
//
// The intermittent swarm-e2e failure
//
//	failed to create service backup_control-plane-backup:
//	  Error response from daemon: network backup_net not found
//
// is that dependency breaking: a leftover backup_net from a previous teardown
// was still mid-removal (Swarm overlay removal is asynchronous), so the deploy
// LISTED it and skipped creating it, then failed to attach a service to a name
// that vanished in between. Creating the network here — with the stack-namespace
// label the deploy looks it up by, so it is REUSED rather than re-created —
// guarantees the network exists before the service that joins it.
//
// Only stacks without an app-level `networks:` block have a private overlay
// (the platform stacks infra/edge/observability/sso all join the shared
// traefik-net/monitoring-net instead); backup is the one that joins
// backup_net.
func ensureStackOverlays(ctx context.Context, d runtime.Client, stack string, composeYAML []byte) ([]string, error) {
	var doc struct {
		Networks map[string]struct {
			External bool `json:"external"`
		} `json:"networks"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("parse compose networks for %s: %w", stack, err)
	}
	var created []string
	for key, net := range doc.Networks {
		if net.External {
			// Shared external overlays (traefik-net, monitoring-net) are
			// created by EnsureBundledNetworks — never (re)created here.
			continue
		}
		name := stack + "_" + key
		isNew, err := ensureStackNetwork(ctx, d, stack, name)
		if err != nil {
			return created, err
		}
		if isNew {
			created = append(created, name)
		}
	}
	return created, nil
}

// ensureStackNetwork guarantees the stack-scoped overlay network `name` exists
// and is stable before the stack that joins it is deployed. It reports whether
// it created the network.
//
// A same-name leftover from a previous teardown that is still mid-removal is
// removed and waited out; a network that is genuinely in use refuses removal,
// which is the correct stable state, and is left for `docker stack deploy` to
// reuse as-is. The freshly created network carries
// com.docker.stack.namespace=<stack> so the deploy detects it as an existing
// stack network and reuses it, instead of colliding with it ("network with
// name <name> already exists").
func ensureStackNetwork(ctx context.Context, d runtime.Client, stack, name string) (bool, error) {
	exists, err := d.NetworkExists(ctx, name)
	if err != nil {
		return false, fmt.Errorf("check network %s: %w", name, err)
	}
	if exists {
		if rmErr := d.NetworkRemove(ctx, name); rmErr != nil {
			// In use by running services — a stable network the deploy will
			// simply reuse. Do not treat the refused removal as an error.
			return false, nil
		}
		if err := waitNetworkGone(ctx, d, name, stackNetGoneTimeout); err != nil {
			return false, fmt.Errorf("network %s did not finish removing: %w", name, err)
		}
	}
	err = d.NetworkCreate(ctx, runtime.NetworkSpec{
		Name:       name,
		Driver:     "overlay",
		Attachable: true,
		Labels:     map[string]string{runtime.StackNamespaceLabel: stack},
	})
	if err != nil {
		return false, fmt.Errorf("create network %s: %w", name, err)
	}
	return true, nil
}
