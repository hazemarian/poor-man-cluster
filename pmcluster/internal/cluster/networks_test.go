package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

func TestEnsureNetwork_CreatesWhenMissing(t *testing.T) {
	f := newFakeDocker()

	created, err := EnsureNetwork(context.Background(), f, "traefik-net")
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if !created {
		t.Error("created = false, want true for a new network")
	}
	if _, ok := f.networks["traefik-net"]; !ok {
		t.Error("network 'traefik-net' not found in fake after creation")
	}
}

func TestEnsureNetwork_NoOpWhenPreExisting(t *testing.T) {
	f := newFakeDocker()

	f.networks["traefik-net"] = runtime.NetworkSpec{Name: "traefik-net", Driver: "overlay", Attachable: true}

	created, err := EnsureNetwork(context.Background(), f, "traefik-net")
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if created {
		t.Error("created = true, want false for a pre-existing network")
	}
}

func TestEnsureNetwork_PropagatesNetworkExistsError(t *testing.T) {
	f := newFakeDocker()
	f.networkExistsErr = errSentinel

	_, err := EnsureNetwork(context.Background(), f, "traefik-net")
	if err == nil {
		t.Fatal("expected error from NetworkExists, got nil")
	}
}

func TestEnsureNetwork_PropagatesNetworkCreateError(t *testing.T) {
	f := newFakeDocker()
	f.networkCreateErr = errSentinel

	_, err := EnsureNetwork(context.Background(), f, "traefik-net")
	if err == nil {
		t.Fatal("expected error from NetworkCreate, got nil")
	}
}

func TestEnsureBundledNetworks_CreatesBoth(t *testing.T) {
	f := newFakeDocker()

	names, err := EnsureBundledNetworks(context.Background(), f)
	if err != nil {
		t.Fatalf("EnsureBundledNetworks: %v", err)
	}

	if len(names) != 2 {
		t.Errorf("created = %v (len %d), want 2 names", names, len(names))
	}

	want := map[string]bool{"traefik-net": false, "monitoring-net": false}
	for _, n := range names {
		if _, ok := want[n]; !ok {
			t.Errorf("unexpected network name %q", n)
		}
		want[n] = true
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("expected network %q in result, not found", n)
		}
	}
}

func TestEnsureBundledNetworks_OnePreExisting(t *testing.T) {
	f := newFakeDocker()

	f.networks["traefik-net"] = runtime.NetworkSpec{Name: "traefik-net", Driver: "overlay", Attachable: true}

	names, err := EnsureBundledNetworks(context.Background(), f)
	if err != nil {
		t.Fatalf("EnsureBundledNetworks: %v", err)
	}

	if len(names) != 1 {
		t.Errorf("created = %v, want [monitoring-net]", names)
	}
	if len(names) > 0 && names[0] != "monitoring-net" {
		t.Errorf("created[0] = %q, want monitoring-net", names[0])
	}
}

// midRemovalClient models the state that produced the intermittent swarm-e2e
// failure: Swarm refuses NetworkRemove because the overlay is already being
// removed asynchronously, and the network then vanishes. Both that state and a
// genuinely in-use network answer the removal with an error, which is why the
// code cannot trust the error alone.
type midRemovalClient struct {
	*fakeDocker
	checks     int
	disappears bool
}

func (c *midRemovalClient) NetworkRemove(_ context.Context, _ string) error {
	return errors.New("network is in use or being removed")
}

func (c *midRemovalClient) NetworkExists(ctx context.Context, name string) (bool, error) {
	c.checks++
	if c.disappears {
		return c.checks == 1, nil
	}
	return c.fakeDocker.NetworkExists(ctx, name)
}

// TestStackNetworkMidRemovalIsRecreated pins the fix for
//
//	failed to create service backup_control-plane-backup:
//	  Error response from daemon: network backup_net not found
//
// A network that refuses removal because it is mid-removal must NOT be treated
// as a stable in-use network: the deploy lists it, skips creating it, and then
// fails to attach a service to a name that no longer exists.
func TestStackNetworkMidRemovalIsRecreated(t *testing.T) {
	defer func(old time.Duration) { networkSettleWindow = old }(networkSettleWindow)
	networkSettleWindow = 40 * time.Millisecond

	fd := newFakeDocker()
	fd.networks["backup_net"] = runtime.NetworkSpec{Name: "backup_net"}
	c := &midRemovalClient{fakeDocker: fd, disappears: true}

	created, err := ensureStackNetwork(context.Background(), c, "backup", "backup_net")
	if err != nil {
		t.Fatalf("ensureStackNetwork: %v", err)
	}
	if !created {
		t.Fatal("a network that vanished mid-removal must be recreated, got created=false")
	}
	if _, ok := fd.networks["backup_net"]; !ok {
		t.Fatal("network was not recreated")
	}
	if got := fd.networks["backup_net"].Labels[runtime.StackNamespaceLabel]; got != "backup" {
		t.Fatalf("recreated network lacks the stack namespace label: %q", got)
	}
}

// TestStackNetworkInUseIsReused is the other half: a network that survives the
// settle window is genuinely in use and must be left alone.
func TestStackNetworkInUseIsReused(t *testing.T) {
	defer func(old time.Duration) { networkSettleWindow = old }(networkSettleWindow)
	networkSettleWindow = 40 * time.Millisecond

	fd := newFakeDocker()
	fd.networks["backup_net"] = runtime.NetworkSpec{Name: "backup_net"}
	c := &midRemovalClient{fakeDocker: fd, disappears: false}

	created, err := ensureStackNetwork(context.Background(), c, "backup", "backup_net")
	if err != nil {
		t.Fatalf("ensureStackNetwork: %v", err)
	}
	if created {
		t.Fatal("an in-use network must be reused, not recreated")
	}
}
