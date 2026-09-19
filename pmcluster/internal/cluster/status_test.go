package cluster

import (
	"context"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/runtime"
)

func TestStatus_PopulatedFromFakeDocker(t *testing.T) {
	f := newFakeDocker()
	f.info = runtime.Info{
		Name:                  "manager-node",
		ServerVersion:         "27.0.0",
		SwarmLocalNodeState:   "active",
		SwarmControlAvailable: true,
		SwarmManagers:         2,
		SwarmNodes:            5,
	}

	rep, err := Status(context.Background(), f)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if rep.NodeName != "manager-node" {
		t.Errorf("NodeName = %q, want 'manager-node'", rep.NodeName)
	}
	if rep.ServerVersion != "27.0.0" {
		t.Errorf("ServerVersion = %q, want '27.0.0'", rep.ServerVersion)
	}
	if rep.SwarmState != "active" {
		t.Errorf("SwarmState = %q, want 'active'", rep.SwarmState)
	}
	if !rep.IsManager {
		t.Error("IsManager = false, want true")
	}
	if rep.NodeCount != 5 {
		t.Errorf("NodeCount = %d, want 5", rep.NodeCount)
	}
	if rep.ManagerCount != 2 {
		t.Errorf("ManagerCount = %d, want 2", rep.ManagerCount)
	}
}

func TestStatus_PreflightNilWhenHealthy(t *testing.T) {
	f := newFakeDocker()
	f.info = goodSwarmInfo()

	rep, err := Status(context.Background(), f)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if rep.Preflight != nil {
		t.Errorf("Preflight = %v, want nil for active manager swarm", rep.Preflight)
	}
}

func TestStatus_PreflightNonNilWhenSwarmInactive(t *testing.T) {
	f := newFakeDocker()
	f.info = runtime.Info{
		Name:                  "solo-node",
		ServerVersion:         "27.0.0",
		SwarmLocalNodeState:   "inactive",
		SwarmControlAvailable: false,
	}

	rep, err := Status(context.Background(), f)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if rep.Preflight == nil {
		t.Error("Preflight = nil, want non-nil for inactive swarm")
	}
}

func TestStatus_ErrorWhenInfoFails(t *testing.T) {
	f := newFakeDocker()
	f.infoErr = errSentinel

	_, err := Status(context.Background(), f)
	if err == nil {
		t.Fatal("Status: expected error when Info fails, got nil")
	}
}
