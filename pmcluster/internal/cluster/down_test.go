package cluster

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
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

	wantStacks := []string{"infra", "edge", "observability", "backup", "sso"}
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

// lingeringNetworkFake simulates Docker's asynchronous overlay network
// removal: after NetworkRemove the network stays inspectable (removing
// state) for a while before it disappears.
type lingeringNetworkFake struct {
	*fakeDocker
	lingerFor time.Duration
	removedAt time.Time
}

func (f *lingeringNetworkFake) NetworkRemove(_ context.Context, name string) error {
	f.removedAt = time.Now()
	return nil
}

func (f *lingeringNetworkFake) NetworkExists(_ context.Context, name string) (bool, error) {
	if f.removedAt.IsZero() {
		return true, nil
	}
	return time.Since(f.removedAt) < f.lingerFor, nil
}

func TestDown_Purge_WaitsForNetworkGone(t *testing.T) {
	f := &lingeringNetworkFake{fakeDocker: newFakeDocker(), lingerFor: 400 * time.Millisecond}
	deployer := &recordingDeployer{}

	start := time.Now()
	res, err := Down(context.Background(), DownDeps{Docker: f, Deployer: deployer, Stdout: io.Discard}, DownInput{Purge: true})
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if len(res.NetworksRemoved) != 2 {
		t.Fatalf("expected 2 networks removed, got %v", res.NetworksRemoved)
	}
	elapsed := time.Since(start)
	if elapsed < 400*time.Millisecond {
		t.Fatalf("expected to wait for the lingering network removal, took %v", elapsed)
	}
}

func TestWaitNetworkGone_TimesOut(t *testing.T) {
	f := &lingeringNetworkFake{fakeDocker: newFakeDocker(), lingerFor: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	f.removedAt = time.Now()
	err := waitNetworkGone(ctx, f, "monitoring-net", 15*time.Second)
	if err == nil {
		t.Fatal("expected timeout error for a network that never disappears")
	}
}

func TestWaitNetworkGone_AlreadyGone(t *testing.T) {
	f := newFakeDocker() // no networks exist
	if err := waitNetworkGone(context.Background(), f, "monitoring-net", time.Second); err != nil {
		t.Fatalf("expected nil for an already-gone network, got %v", err)
	}
}

// TestDown_Purge_WritesBackupAndDeletesStore verifies `--purge` now backs up the
// local store (data.db + .encryption_key + config.yaml) to a restorable tarball
// and then deletes the store files — while keeping the backup archive.
func TestDown_Purge_WritesBackupAndDeletesStore(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"data.db":         "sqlite-bytes",
		".encryption_key": "key-bytes",
		"config.yaml":     "data_dir: /tmp\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	f := newFakeDocker()
	f.info = goodSwarmInfo()
	deployer := &recordingDeployer{}

	res, err := Down(cancelledCtx(), DownDeps{Docker: f, Deployer: deployer, Stdout: io.Discard}, DownInput{Purge: true, StoreDir: dir})
	if err != nil {
		t.Fatalf("Down (purge): %v", err)
	}

	if res.StoreBackupPath == "" {
		t.Fatal("StoreBackupPath empty after purge with StoreDir")
	}
	if _, err := os.Stat(res.StoreBackupPath); err != nil {
		t.Fatalf("backup archive not found: %v", err)
	}

	entries := tarEntries(t, res.StoreBackupPath)
	for _, want := range []string{"data.db", ".encryption_key", "config.yaml"} {
		if !entries[want] {
			t.Errorf("backup archive missing %q (entries=%v)", want, entries)
		}
	}

	for _, name := range []string{"data.db", ".encryption_key", "config.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("store file %s should be deleted (stat err=%v)", name, err)
		}
	}
}

// TestDown_NoPurge_NoBackupNoDelete verifies `cluster down` WITHOUT --purge
// writes no backup and deletes nothing from the store.
func TestDown_NoPurge_NoBackupNoDelete(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFakeDocker()
	deployer := &recordingDeployer{}

	res, err := Down(context.Background(), DownDeps{Docker: f, Deployer: deployer, Stdout: io.Discard}, DownInput{Purge: false, StoreDir: dir})
	if err != nil {
		t.Fatalf("Down (no purge): %v", err)
	}
	if res.StoreBackupPath != "" {
		t.Errorf("no backup expected without purge, got %q", res.StoreBackupPath)
	}
	if _, err := os.Stat(filepath.Join(dir, "data.db")); err != nil {
		t.Errorf("data.db should be preserved without purge: %v", err)
	}
}
