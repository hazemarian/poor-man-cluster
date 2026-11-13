package cluster

import (
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

// TestNodeStatOTLPEndpointIsTheLeadersLoopback is the daemon half of the same
// contract. The collector follows the leader and publishes 4318 there only, so exactly
// one daemon exports — to loopback, beside the collector. Every other node reports to the
// leader over mTLS and exports nothing; a probe that reported that as a failure is what
// this replaced.
func TestNodeStatOTLPEndpointIsTheLeadersLoopback(t *testing.T) {
	endpoint, reason := config.ResolveOTLPEndpoint(config.OTLPAutoEndpoint, true)
	if reason != "" {
		t.Fatalf("the leader's endpoint was disabled: %s", reason)
	}
	if endpoint != "http://127.0.0.1:4318" {
		t.Fatalf("endpoint = %q, want the collector beside the leader at http://127.0.0.1:4318", endpoint)
	}
	// A worker exports nothing, and says why without calling it a fault.
	ep, why := config.ResolveOTLPEndpoint(config.OTLPAutoEndpoint, false)
	if ep != "" || why == "" {
		t.Fatalf("worker endpoint = (%q, %q), want none with a reason", ep, why)
	}
}
