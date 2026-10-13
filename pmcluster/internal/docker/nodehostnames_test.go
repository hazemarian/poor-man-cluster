package docker

import (
	"testing"

	"github.com/docker/docker/api/types/swarm"
)

// TestNodeHostnamesUsesTheDescription guards the field choice behind the
// running-node column. Spec.Annotations.Name is empty in practice on this
// cluster's nodes, so keying the join on it leaves every service's running node
// blank — a failure with no error and no log, only an empty column.
func TestNodeHostnamesUsesTheDescription(t *testing.T) {
	got := nodeHostnames([]swarm.Node{
		{ID: "id-1", Description: swarm.NodeDescription{Hostname: "nxt-sw-1-m"}},
		// Annotation-only node: tolerated as a fallback, never preferred.
		{ID: "id-2", Spec: swarm.NodeSpec{Annotations: swarm.Annotations{Name: "fallback-node"}}},
		{ID: "id-3"},
	})
	if got["id-1"] != "nxt-sw-1-m" {
		t.Errorf("id-1 = %q, want the Description hostname nxt-sw-1-m", got["id-1"])
	}
	if got["id-2"] != "fallback-node" {
		t.Errorf("id-2 = %q, want the annotation fallback", got["id-2"])
	}
	if got["id-3"] != "" {
		t.Errorf("id-3 = %q, want empty", got["id-3"])
	}
}
