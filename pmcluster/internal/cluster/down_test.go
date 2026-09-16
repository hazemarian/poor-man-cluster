package cluster

import (
	"context"
	"io"
	"testing"
)

// cancelledCtx returns a context that is already cancelled so that
// waitTeardownSettle returns immediately without sleeping 5 seconds.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func makeDownDeps(f *fakeDocker, deployer *recordingDeployer) DownDeps {
	return DownDeps{
		Docker:   f,
		Deployer: deployer,
		Stdout:   io.Discard,
	}
}

func TestDown_RemovesThreeStacks(t *testing.T) {
	f := newFakeDocker()
	deployer := &recordingDeployer{}

	res, err := Down(context.Background(), makeDownDeps(f, deployer), DownInput{Purge: false})
	if err != nil {
		t.Fatalf("Down: %v", err)
	}

	wantStacks := []string{"infra", "edge", "observability", "backup"}
	if len(res.StacksRemoved) != len(wantStacks) {
		t.Fatalf("StacksRemoved = %v, want %v", res.StacksRemoved, wantStacks)
	}
	for i, want := range wantStacks {
		if res.StacksRemoved[i] != want {
			t.Errorf("StacksRemoved[%d] = %q, want %q", i, res.StacksRemoved[i], want)
		}
	}
}

func TestDown_NoPurge_PreservesSecretsConfigsNetworks(t *testing.T) {
	f := newFakeDocker()

	f.secrets["admin_credentials"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "admin_credentials"}
	f.networks["traefik-net"] = struct {
		Name       string
		Driver     string
		Attachable bool
	}{Name: "traefik-net"}

	deployer := &recordingDeployer{}
	res, err := Down(context.Background(), makeDownDeps(f, deployer), DownInput{Purge: false})
	if err != nil {
		t.Fatalf("Down (no purge): %v", err)
	}

	if len(res.SecretsRemoved) > 0 {
		t.Errorf("secrets removed without --purge: %v", res.SecretsRemoved)
	}
	if len(res.ConfigsRemoved) > 0 {
		t.Errorf("configs removed without --purge: %v", res.ConfigsRemoved)
	}
	if len(res.NetworksRemoved) > 0 {
		t.Errorf("networks removed without --purge: %v", res.NetworksRemoved)
	}

	if _, ok := f.secrets["admin_credentials"]; !ok {
		t.Error("admin_credentials secret removed without --purge")
	}
	if _, ok := f.networks["traefik-net"]; !ok {
		t.Error("traefik-net network removed without --purge")
	}
}

func TestDown_Purge_RemovesAllManagedResources(t *testing.T) {
	f := newFakeDocker()
	f.info = goodSwarmInfo()

	for _, name := range pmclusterManagedSecrets {
		f.secrets[name] = struct {
			Name   string
			Data   []byte
			Labels map[string]string
		}{Name: name}
	}

	f.configs["pmcluster_otel_config_v001"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "pmcluster_otel_config_v001", Labels: map[string]string{pmclusterLabel: "true"}}
	f.configs["pmcluster_traefik_dynamic_v001"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "pmcluster_traefik_dynamic_v001", Labels: map[string]string{pmclusterLabel: "true"}}
	for _, name := range pmclusterManagedNetworks {
		f.networks[name] = struct {
			Name       string
			Driver     string
			Attachable bool
		}{Name: name}
	}

	deployer := &recordingDeployer{}

	res, err := Down(cancelledCtx(), makeDownDeps(f, deployer), DownInput{Purge: true})
	if err != nil {
		t.Fatalf("Down (purge): %v", err)
	}

	if len(res.SecretsRemoved) != len(pmclusterManagedSecrets) {
		t.Errorf("SecretsRemoved = %v (len %d), want %d",
			res.SecretsRemoved, len(res.SecretsRemoved), len(pmclusterManagedSecrets))
	}

	if len(res.ConfigsRemoved) != 2 {
		t.Errorf("ConfigsRemoved = %v (len %d), want 2",
			res.ConfigsRemoved, len(res.ConfigsRemoved))
	}

	if len(res.NetworksRemoved) != len(pmclusterManagedNetworks) {
		t.Errorf("NetworksRemoved = %v (len %d), want %d",
			res.NetworksRemoved, len(res.NetworksRemoved), len(pmclusterManagedNetworks))
	}
}

// TestDown_Idempotent verifies that Down on an already-empty docker does not
// error. The fake SecretRemove/ConfigRemove/NetworkRemove on the fakeDocker
// always succeed (they no-op on missing names), mirroring the production
// client's idempotent behaviour.
func TestDown_Idempotent(t *testing.T) {
	f := newFakeDocker()
	deployer := &recordingDeployer{}

	_, err := Down(cancelledCtx(), makeDownDeps(f, deployer), DownInput{Purge: true})
	if err != nil {
		t.Fatalf("Down (empty docker, purge): %v", err)
	}
}

// TestDown_Idempotent_NoPurge verifies Down without purge on an empty docker
// doesn't error either.
func TestDown_Idempotent_NoPurge(t *testing.T) {
	f := newFakeDocker()
	deployer := &recordingDeployer{}

	_, err := Down(context.Background(), makeDownDeps(f, deployer), DownInput{Purge: false})
	if err != nil {
		t.Fatalf("Down (empty docker, no purge): %v", err)
	}
}
