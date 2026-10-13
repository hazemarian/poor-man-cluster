package stacks

import (
	"context"
	"errors"
	"testing"
)

// newPinRecorder returns a PinResolver with in-memory pins, mirroring how the
// daemon wires it (StackPin reads, SetStackPin writes).
func newPinRecorder(pins map[string]string, nodes ...string) *PinResolver {
	return &PinResolver{
		StorageNodes: nodes,
		StackPin: func(_ context.Context, stackName string) (string, error) {
			return pins[stackName], nil
		},
		SetStackPin: func(_ context.Context, stackName, node string) error {
			pins[stackName] = node
			return nil
		},
	}
}

// TestPersistAssignmentMakesTheFirstPlacementSticky is the regression guard for
// the contract that a stack is assigned a storage node once. Without the
// recorded pin the resolver re-runs the round-robin on every deploy, so growing
// storage_nodes relocates every stack whose index changes onto a node that
// holds none of its data.
func TestPersistAssignmentMakesTheFirstPlacementSticky(t *testing.T) {
	pins := map[string]string{}
	r := newPinRecorder(pins, "n1", "n2", "n3")
	ir := testIR("erp", st("web", []string{"erp-data:/data"}, ""))
	ctx := context.Background()

	if err := r.PersistAssignment(ctx, "erp", ir); err != nil {
		t.Fatalf("PersistAssignment: %v", err)
	}
	first := pins["erp"]
	if first == "" {
		t.Fatal("no pin recorded: the next storage_nodes change would relocate the stack")
	}

	// The pin must name the node the resolver actually placed the service on.
	placed := testIR("erp", st("web", []string{"erp-data:/data"}, ""))
	if err := r.ResolvePlacement(ctx, "erp", placed); err != nil {
		t.Fatalf("ResolvePlacement: %v", err)
	}
	if got := placed.Services[0].Placement; got != first {
		t.Fatalf("pin %q does not match the resolved placement %q", first, got)
	}

	// Growing the storage-node list must not rewrite an existing assignment.
	r.SetStorageNodes([]string{"n1", "n2", "n3", "n4"})
	if err := r.PersistAssignment(ctx, "erp", ir); err != nil {
		t.Fatalf("PersistAssignment (second): %v", err)
	}
	if pins["erp"] != first {
		t.Fatalf("pin rewritten %q -> %q when the node list changed; assignments must be sticky", first, pins["erp"])
	}
}

// A stateless stack has nothing that ties it to a node, so it stays free.
func TestPersistAssignmentLeavesStatelessStacksFree(t *testing.T) {
	pins := map[string]string{}
	r := newPinRecorder(pins, "n1", "n2", "n3")
	ir := testIR("site", st("web", nil, ""))

	if err := r.PersistAssignment(context.Background(), "site", ir); err != nil {
		t.Fatalf("PersistAssignment: %v", err)
	}
	if _, ok := pins["site"]; ok {
		t.Fatal("a stateless stack was pinned; it should stay free to land anywhere")
	}
}

// With no storage_nodes configured the platform_node fallback is already
// stable, so there is nothing to record.
func TestPersistAssignmentNoopsWithoutStorageNodes(t *testing.T) {
	pins := map[string]string{}
	r := newPinRecorder(pins)
	ir := testIR("erp", st("web", []string{"erp-data:/data"}, ""))

	if err := r.PersistAssignment(context.Background(), "erp", ir); err != nil {
		t.Fatalf("PersistAssignment: %v", err)
	}
	if len(pins) != 0 {
		t.Fatalf("pins written without storage nodes: %v", pins)
	}
}

// A failure to record must surface, not be swallowed: a silent failure leaves
// the stack looking placed while remaining free to relocate.
func TestPersistAssignmentPropagatesWriteErrors(t *testing.T) {
	sentinel := errors.New("store down")
	r := &PinResolver{
		StorageNodes: []string{"n1", "n2"},
		StackPin:     func(_ context.Context, _ string) (string, error) { return "", nil },
		SetStackPin:  func(_ context.Context, _, _ string) error { return sentinel },
	}
	ir := testIR("erp", st("web", []string{"erp-data:/data"}, ""))
	err := r.PersistAssignment(context.Background(), "erp", ir)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the write error to propagate, got %v", err)
	}
}
