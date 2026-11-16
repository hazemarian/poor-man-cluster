//go:build e2e

package e2e

import (
	"context"
	"strings"
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

// ensurePlatformNetworks ensures both networks the platform stacks expect and schedules removal of
// exactly the ones this test created.
func ensurePlatformNetworks(t *testing.T, ctx context.Context) {
	t.Helper()
	var created []string
	for _, name := range []string{"traefik-net", "monitoring-net"} {
		if ensureOverlayNetwork(t, ctx, name) {
			created = append(created, name)
		}
	}
	if len(created) == 0 {
		return
	}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, name := range created {
			out, err := dockerRun(cleanCtx, "network", "rm", name)
			if err != nil {
				t.Logf("docker network rm %s: %v\n%s", name, err, out)
			}
		}
	})
}
