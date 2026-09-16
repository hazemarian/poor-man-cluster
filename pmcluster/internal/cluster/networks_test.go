package cluster

import (
	"context"
	"testing"
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

	f.networks["traefik-net"] = struct {
		Name       string
		Driver     string
		Attachable bool
	}{Name: "traefik-net", Driver: "overlay", Attachable: true}

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

	f.networks["traefik-net"] = struct {
		Name       string
		Driver     string
		Attachable bool
	}{Name: "traefik-net", Driver: "overlay", Attachable: true}

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
