package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// TestApplyRotatedCredential_TriggersClusterUpdate is the BUG-012 regression
// test: credential rotation must apply via the cluster update pipeline so
// dependent rendered configs (OTel basic-auth header) and stacks
// (observability) pick up the new password immediately — NOT after a manual
// `cluster update`.
func TestApplyRotatedCredential_TriggersClusterUpdate(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()

	st, err := store.Open(t.TempDir() + "/data.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	orig := credsUpdateFn
	defer func() { credsUpdateFn = orig }()

	var called bool
	credsUpdateFn = func(ctx context.Context, deps cluster.UpdateDeps, in cluster.UpdateInput) (*cluster.UpdateResult, error) {
		called = true
		if in.ConfigDir == "" {
			t.Errorf("UpdateInput.ConfigDir is empty; want the store config dir")
		}
		if in.Version == "" {
			t.Errorf("UpdateInput.Version is empty; want buildinfo.Version")
		}
		if deps.Store == nil {
			t.Errorf("UpdateDeps.Store is nil; want the open store")
		}
		return &cluster.UpdateResult{}, nil
	}

	if _, err := applyRotatedCredential(context.Background(), cluster.UpdateDeps{Store: st}, cluster.UpdateInput{ConfigDir: "/tmp/pmc-test", Version: "v0.2.151"}); err != nil {
		t.Fatalf("applyRotatedCredential: %v", err)
	}
	if !called {
		t.Fatal("BUG-012: credential rotation did NOT trigger the cluster update pipeline (dependent configs/stacks would keep the old password)")
	}
}

// TestApplyRotatedCredential_ErrorSurfaces propagates update failures with a
// retry hint (run `pmcluster cluster update` to retry).
func TestApplyRotatedCredential_ErrorSurfaces(t *testing.T) {
	orig := credsUpdateFn
	defer func() { credsUpdateFn = orig }()

	credsUpdateFn = func(context.Context, cluster.UpdateDeps, cluster.UpdateInput) (*cluster.UpdateResult, error) {
		return nil, errors.New("simulated update failure")
	}

	if _, err := applyRotatedCredential(context.Background(), cluster.UpdateDeps{}, cluster.UpdateInput{}); err == nil {
		t.Fatal("expected error to propagate from credsUpdateFn")
	}
}
