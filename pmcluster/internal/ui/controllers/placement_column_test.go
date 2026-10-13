package controllers

import (
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// TestServiceRowCarriesTheRunningNode guards the console's placement column. A
// service row shows where the service was ASSIGNED (the io.pmcluster.node
// label) and where it actually IS (its running task's node) — the second is
// what makes a service running away from its data visible instead of silent.
func TestServiceRowCarriesTheRunningNode(t *testing.T) {
	assigned := newServiceRow(pmapi.Service{
		Name: "erp_web", Stack: "erp", Node: "nxt-sw-1-m", RunningNode: "nxt-sw-4-m",
	})
	if assigned.Node != "nxt-sw-1-m" || assigned.RunningNode != "nxt-sw-4-m" {
		t.Fatalf("assigned row lost a node: Node=%q RunningNode=%q", assigned.Node, assigned.RunningNode)
	}

	// A stateless service has no assigned node; it must still report where it is.
	free := newServiceRow(pmapi.Service{Name: "cms_web", Stack: "cms", RunningNode: "nxt-sw-4-m"})
	if free.Node != "" || free.RunningNode != "nxt-sw-4-m" {
		t.Fatalf("unassigned row lost its running node: Node=%q RunningNode=%q", free.Node, free.RunningNode)
	}

	// Nothing running and nothing assigned stays empty rather than inventing a node.
	idle := newServiceRow(pmapi.Service{Name: "job_migration", Stack: "job"})
	if idle.Node != "" || idle.RunningNode != "" {
		t.Fatalf("idle row invented a node: Node=%q RunningNode=%q", idle.Node, idle.RunningNode)
	}
}
