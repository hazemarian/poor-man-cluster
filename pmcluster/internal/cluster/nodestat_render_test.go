package cluster

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
)

// ndCollectorDoc is the shape of the rendered observability stack that the
// node-stats panel depends on.
type ndCollectorDoc struct {
	Services map[string]struct {
		Mode    string   `json:"mode"`
		Volumes []string `json:"volumes"`
		Deploy  struct {
			Mode      string `json:"mode"`
			Replicas  *int   `json:"replicas"`
			Placement struct {
				Constraints []string `json:"constraints"`
				Preferences []struct {
					Spread string `json:"spread"`
				} `json:"preferences"`
			} `json:"placement"`
		} `json:"deploy"`
	} `json:"services"`
}

func renderNodeStatStack(t *testing.T) ndCollectorDoc {
	t.Helper()
	body, err := LoadComposeFile(StackObservability, RenderInput{
		Domain:                     "example.com",
		ObservabilityAdminEmail:    "admin@example.com",
		ObservabilityAdminPassword: "not-a-real-password",
		LeaderNode:                 "nxt-sw-5-m",
	})
	if err != nil {
		t.Fatalf("render observability stack: %v", err)
	}
	var doc ndCollectorDoc
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("rendered observability stack is not valid YAML: %v", err)
	}
	return doc
}

// TestOTelCollectorIsSingleTaskPinnedToTheLeader pins the shape: the collector is
// no longer `global`. It is one task on a manager, with the leader as a HARD
// CONSTRAINT — never a preference. A preference only orders the nodes that
// already satisfy every constraint and leaves a running task where it is, which is
// how the collector stayed on a former leader and the node-stats panel stayed
// empty (measured on the fleet: 5-m carried pmcluster.leader, the task sat on 4-m).
func TestOTelCollectorIsSingleTaskPinnedToTheLeader(t *testing.T) {
	doc := renderNodeStatStack(t)
	svc, ok := doc.Services["otel-collector"]
	if !ok {
		t.Fatal("rendered stack has no otel-collector service")
	}

	if svc.Mode == "global" || svc.Deploy.Mode == "global" {
		t.Fatal("otel-collector is still mode: global; it must be a single task")
	}
	if svc.Deploy.Replicas == nil || *svc.Deploy.Replicas != 1 {
		t.Fatalf("otel-collector replicas = %v, want exactly 1", svc.Deploy.Replicas)
	}

	// Placement is a NAME now, not the leader label: a label constraint floats with
	// leadership and leaves the task at 0/1 when the label moves without a redeploy.
	var pinned string
	for _, c := range svc.Deploy.Placement.Constraints {
		if strings.Contains(c, "pmcluster.leader") {
			t.Fatalf("otel-collector is still placed by the leader LABEL (%q): that floats", c)
		}
		if v, ok := strings.CutPrefix(strings.ReplaceAll(c, " ", ""), "node.hostname=="); ok {
			pinned = v
		}
	}
	if pinned == "" {
		t.Fatalf("otel-collector constraints = %v, want it pinned to the leader by hostname",
			svc.Deploy.Placement.Constraints)
	}
	// And no preference on the label either: a preference beside a name would let the
	// scheduler keep a task on a node the name no longer selects.
	for _, p := range svc.Deploy.Placement.Preferences {
		if strings.Contains(p.Spread, "pmcluster.leader") {
			t.Errorf("otel-collector still has a preference on the leader label (%q)", p.Spread)
		}
	}
}

// TestNodeStatOTLPEndpointIsTheNodesOwnSwarmAddress is the daemon half of the
// same contract: the sample is exported to THIS node's swarm address, never to
// localhost (a closed port on every node but the collector's) and never to the
// overlay service name (a host process cannot resolve it).
func TestNodeStatOTLPEndpointIsTheNodesOwnSwarmAddress(t *testing.T) {
	endpoint, reason := config.ResolveOTLPEndpoint(context.Background(), config.OTLPAutoEndpoint,
		func(context.Context) (string, error) { return "10.7.224.15", nil },
		func(context.Context, string) error { return nil })
	if reason != "" {
		t.Fatalf("endpoint disabled: %s", reason)
	}
	if endpoint != "http://10.7.224.15:4318" {
		t.Fatalf("endpoint = %q, want http://10.7.224.15:4318", endpoint)
	}
	for _, bad := range []string{"127.0.0.1", "localhost", "observability_otel-collector"} {
		if strings.Contains(endpoint, bad) {
			t.Fatalf("endpoint %q names %q", endpoint, bad)
		}
	}
	// And the port it dials is the one the stack publishes.
	if config.OTLPPort != 4318 {
		t.Fatalf("OTLPPort = %d, want 4318", config.OTLPPort)
	}
}
