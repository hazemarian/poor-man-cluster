package controllers

import (
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
	"testing"
)

func TestObservabilityHealthyZeroAndUnknownJob(t *testing.T) {
	var h observabilityHealth
	projectServices([]pmapi.Service{{Name: "demo_web", Stack: "demo", Replicas: 1, Desired: 1}}, &h)
	if !h.DeficitKnown || h.ReplicaDeficit != 0 || !h.Services[0].DeficitKnown || h.ServicesReady != 1 {
		t.Fatalf("healthy zero: %+v", h)
	}
	projectServices([]pmapi.Service{{Name: "demo_migration", Stack: "demo", RunOnce: true, Replicas: 0, Desired: 1}}, &h)
	if h.DeficitKnown || h.Services[1].Completed || !h.Services[1].Job || h.ServicesTotal != 1 {
		t.Fatalf("invented job completion: %+v", h)
	}
}
