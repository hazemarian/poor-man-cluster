package cli

import (
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// TestOrphanServicesKeepsOnlyStackless pins the whole selection rule: a service
// with a stack namespace is never a candidate, whatever else is true of it —
// including a stack that no longer exists, where removing the service is the
// stack's own re-deploy to settle.
func TestOrphanServicesKeepsOnlyStackless(t *testing.T) {
	services := []runtime.Service{
		{Name: "edge_pmcluster-edge", Stack: "edge", Replicas: 1, Desired: 1},
		{Name: "erp-seed-run", Stack: "", Replicas: 0, Desired: 1, RunOnce: true},
		{Name: "demoenv_web", Stack: "demoenv", Replicas: 1, Desired: 1},
		{Name: "hand-made", Stack: "", Replicas: 1, Desired: 1},
	}

	got := orphanServices(services)

	if len(got) != 2 {
		t.Fatalf("got %d orphan(s), want 2: %+v", len(got), got)
	}
	// Order must follow the client's listing so repeated runs report the same
	// way; a map-backed implementation would not.
	if got[0].Name != "erp-seed-run" || got[1].Name != "hand-made" {
		t.Errorf("orphans = %v, want [erp-seed-run hand-made]", []string{got[0].Name, got[1].Name})
	}
}

func TestOrphanServicesEmptyWhenAllStacked(t *testing.T) {
	got := orphanServices([]runtime.Service{
		{Name: "a_x", Stack: "a"},
		{Name: "b_y", Stack: "b"},
	})
	if len(got) != 0 {
		t.Errorf("got %d orphan(s), want 0", len(got))
	}
}

// TestReplicaStateNamesACompletedRunOnce keeps a finished one-shot from being
// reported as the alarming "0/1".
func TestReplicaStateNamesACompletedRunOnce(t *testing.T) {
	if got := replicaState(runtime.Service{Replicas: 0, Desired: 1, RunOnce: true}); got != "0/1 (run-once)" {
		t.Errorf("run-once = %q, want %q", got, "0/1 (run-once)")
	}
	if got := replicaState(runtime.Service{Replicas: 0, Desired: 1}); got != "0/1" {
		t.Errorf("plain = %q, want %q", got, "0/1")
	}
	if got := replicaState(runtime.Service{Replicas: 2, Desired: 2}); got != "2/2" {
		t.Errorf("healthy = %q, want %q", got, "2/2")
	}
}
