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

	const leaderConstraint = "node.labels." + "pmcluster.leader==true"
	var haveManager, haveLeader bool
	for _, c := range svc.Deploy.Placement.Constraints {
		switch c {
		case "node.role == manager":
			haveManager = true
		case leaderConstraint:
			haveLeader = true
		}
	}
	if !haveManager {
		t.Fatalf("otel-collector constraints = %v, want node.role == manager as the floor",
			svc.Deploy.Placement.Constraints)
	}
	if !haveLeader {
		t.Fatalf("otel-collector constraints = %v, want the leader as a CONSTRAINT (%s): a preference "+
			"does not move a running task", svc.Deploy.Placement.Constraints, leaderConstraint)
	}

	// And the label must not ALSO be a preference: the two together would let the
	// scheduler keep a task on a node the constraint no longer selects, which is
	// the bug this pins.
	for _, p := range svc.Deploy.Placement.Preferences {
		if strings.Contains(p.Spread, "pmcluster.leader") {
			t.Errorf("otel-collector still has a preference on the leader label (%q); it must be a constraint only", p.Spread)
		}
	}
}

// TestOTelCollectorMountsTheNodeStatDirectory: the collector writes the JSONL
// under /var/stack, and the leader's daemon (a host process) reads it from the
// same path. Without this bind the file lives only inside the container.
func TestOTelCollectorMountsTheNodeStatDirectory(t *testing.T) {
	doc := renderNodeStatStack(t)
	svc := doc.Services["otel-collector"]
	want := "/var/stack/nodestat:/var/stack/nodestat"
	for _, b := range svc.Volumes {
		if b == want {
			return
		}
	}
	t.Fatalf("otel-collector volumes = %v, want %q", svc.Volumes, want)
}

// TestOTelCollectorHasTheNodeStatPipeline pins the collector-side contract:
// node-stats series land in a JSONL file with bounded rotation, and are kept out
// of the general metrics pipeline so Parseable does not ingest them twice.
func TestOTelCollectorHasTheNodeStatPipeline(t *testing.T) {
	data, err := RenderOTelCollectorConfig(RenderInput{
		ObservabilityAdminEmail: "admin@example.com",
		ObservabilityBasicAuth:  observabilityBasicAuth("admin@example.com", "pw"),
	})
	if err != nil {
		t.Fatalf("render collector config: %v", err)
	}
	body := string(data)

	for _, want := range []string{
		"file/nodestat:",
		"path: /var/stack/nodestat/nodestat.jsonl",
		"format: json",
		"max_megabytes: 2",
		// One backup, not zero: with 0 the exporter renames the rolled file and
		// never deletes it, which grew a live leader's directory to 722 MiB
		// across 372 files while the reader tailed only the newest.
		"max_backups: 1",
		"metrics/nodestat:",
		"exporters:  [file/nodestat, otlphttp/parseable-metrics]",
		"filter/nodestat_only:",
		"filter/drop_nodestat:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("collector config is missing %q", want)
		}
	}

	// The file pipeline must carry ONLY node-stats series: the reader would
	// otherwise carry application metrics into the table it builds.
	if !strings.Contains(body, `^pmcluster\.node\..*`) || !strings.Contains(body, `^pmcluster\.process\..*`) {
		t.Error("node-stats filter does not name the node and process metric families")
	}

	// The general metrics pipeline must still lead with memory_limiter and end
	// with batch, and must drop the node-stats series now that they have their
	// own pipeline.
	start := strings.Index(body, "    metrics:")
	if start < 0 {
		t.Fatal("no general metrics pipeline")
	}
	end := strings.Index(body[start:], "    metrics/nodestat:")
	if end < 0 {
		t.Fatal("no metrics/nodestat pipeline after the general one")
	}
	pipe := body[start : start+end]
	if !strings.Contains(pipe, "[memory_limiter,") {
		t.Error("metrics pipeline must lead with memory_limiter")
	}
	if !strings.Contains(pipe, "filter/drop_nodestat") || !strings.Contains(pipe, ", batch]") {
		t.Error("metrics pipeline must drop node-stats series and end with batch")
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
