//go:build e2e

package e2e

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// ensureOverlayNetwork creates an attachable overlay network when it is missing, and reports
// whether THIS call created it.
//
// The reporting half is the point. Every swarm test that needs traefik-net/monitoring-net used to
// create them if absent and then `docker network rm` them in cleanup — so whichever test finished
// first took the networks out from under the ones still running. In one long job that stayed
// hidden behind the old ordering; split into separate groups it surfaced immediately as
// `network "monitoring-net" is declared as external, but could not be found`. A test removes only
// what it created.
func ensureOverlayNetwork(t *testing.T, ctx context.Context, name string) (created bool) {
	t.Helper()
	netCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := dockerRun(netCtx, "network", "create", "--driver=overlay", "--attachable", name)
	if err != nil {
		if strings.Contains(out, "already exists") || strings.Contains(err.Error(), "already exists") {
			t.Logf("network %s already exists — reusing", name)
			return false
		}
		t.Fatalf("docker network create %s: %v\n%s", name, err, out)
	}
	t.Logf("created overlay network: %s", name)
	return true
}

// ensurePlatformNetworks creates the networks the platform stacks expect, ONCE PER PROCESS, and
// deliberately never removes them.
//
// They belong to the JOB, not to a test. Removing them in a test's cleanup deleted infrastructure
// that the next test in the same job still needed — measured: TestDeployPipeline created both,
// finished, removed both, and TestSecretsConfigsE2E then deployed into
// `network "traefik-net" is declared as external, but could not be found`. The runner is
// ephemeral and the swarm dies with it, so leaving them behind is correct; prod leaves them too.
var platformNetworksOnce sync.Once

func ensurePlatformNetworks(t *testing.T, ctx context.Context) {
	t.Helper()
	platformNetworksOnce.Do(func() {
		for _, name := range []string{"traefik-net", "monitoring-net"} {
			ensureOverlayNetwork(t, ctx, name)
		}
	})
}
