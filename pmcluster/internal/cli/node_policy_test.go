package cli

import (
	"context"
	"os"
	"testing"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// TestApplyNodePolicyOnceFollowsTheSetting is the leader case measured on
// 2026-11-14: `settings set node_docker_restart=true` reached a follower through
// its runner agent within 50s while the leader's own policy still said false,
// because its only write happened at boot. The leader owns the setting, so it is
// the node that must follow it.
func TestApplyNodePolicyOnceFollowsTheSetting(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{DataDir: t.TempDir()}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.SetSetting(ctx, cluster.SettingNodeDockerRestart(), "true"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	_ = st.Close()

	if !applyNodePolicyOnce(ctx, cfg, zerolog.Nop()) {
		t.Fatal("policy not written on the first pass")
	}
	if !runners.ReadNodePolicy(cfg.DataDir).DockerRestart {
		t.Fatal("policy does not arm the docker restart")
	}
	// Idempotent: a pass that changes nothing must not rewrite the file.
	if applyNodePolicyOnce(ctx, cfg, zerolog.Nop()) {
		t.Error("second pass rewrote an unchanged policy")
	}

	// Turning it back off is the same path, in reverse.
	st, err = store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.SetSetting(ctx, cluster.SettingNodeDockerRestart(), "false"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	_ = st.Close()
	if !applyNodePolicyOnce(ctx, cfg, zerolog.Nop()) {
		t.Fatal("disarm was not written")
	}
	if runners.ReadNodePolicy(cfg.DataDir).DockerRestart {
		t.Fatal("policy still arms the docker restart")
	}
}

// A store that holds no decision (a worker's, or a follower that has not mirrored
// the row) must leave the policy alone: writing the default there disarmed a node
// during the dockerd outage the watchdog existed for.
func TestApplyNodePolicyOnceLeavesUnknownAlone(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{DataDir: t.TempDir()}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	_ = st.Close()

	if applyNodePolicyOnce(ctx, cfg, zerolog.Nop()) {
		t.Fatal("wrote a policy from a store with no decision")
	}
	if _, statErr := os.Stat(runners.NodePolicyPath(cfg.DataDir)); statErr == nil {
		t.Fatal("a policy file was created where none should be")
	}

	// And no store at all (a node that was never initialised) is the same answer.
	empty := &config.Config{DataDir: t.TempDir()}
	if applyNodePolicyOnce(ctx, empty, zerolog.Nop()) {
		t.Fatal("wrote a policy without a store")
	}
}
