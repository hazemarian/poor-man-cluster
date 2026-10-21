package cluster

import (
	"strings"
	"testing"
)

// TestTraefikRunsOnManagers: traefik is global but constrained to managers,
// because its swarm provider can only list services through a manager's Docker
// socket. A placement-free global traefik would also run on workers (where the
// provider fails, leaving no routers) and the ingress routing mesh would
// intermittently route requests to a routerless replica (prod SSO 404s).
func TestTraefikRunsOnManagers(t *testing.T) {
	in := RenderInput{Domain: "example.com", EdgeImage: "ghcr.io/hazemarian/pmcluster-edge:v0.2.87"}
	out, err := LoadComposeFile(StackInfra, in)
	if err != nil {
		t.Fatalf("%v", err)
	}
	s := string(out)
	if !strings.Contains(s, "mode: global") {
		t.Errorf("traefik not global:\n%s", s)
	}
	if !strings.Contains(s, "node.role == manager") {
		t.Errorf("traefik must be pinned to manager nodes:\n%s", s)
	}
	if strings.Contains(s, "node.hostname") {
		t.Errorf("traefik must not carry a node-hostname pin:\n%s", s)
	}
}

// TestPlatformStackPlacements pins the per-stack rule. One global "platform
// node" was wrong for all of them, because the stacks differ in whether they
// own data:
//
//	observability -> the resolved storage node (Parseable keeps a host-path store)
//	backup        -> a worker (the durable copy leaves the storage failure domain)
//	edge          -> the current leader (it is the API host)
//	sso           -> role only (no data at all)
func TestPlatformStackPlacements(t *testing.T) {
	in := RenderInput{
		Domain:            "example.com",
		EdgeImage:         "ghcr.io/hazemarian/pmcluster-edge:v0.2.87",
		EdgeNode:          "leader-1",
		ObservabilityNode: "storage-1",
		BackupNode:        "worker-1",
	}

	for _, tc := range []struct {
		name  string
		stack stackName
		want  string
	}{
		{"observability", StackObservability, "node.hostname == storage-1"},
		{"backup", StackBackup, "node.hostname == worker-1"},
		{"edge", StackEdge, "node.hostname == leader-1"},
	} {
		out, err := LoadComposeFile(tc.stack, in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !strings.Contains(string(out), tc.want) {
			t.Fatalf("%s: expected %q in the rendered compose:\n%s", tc.name, tc.want, out)
		}
	}

	// SSO holds no data, so it stays role-based and carries no hostname.
	out, err := LoadComposeFile(StackSSO, in)
	if err != nil {
		t.Fatalf("sso: %v", err)
	}
	if strings.Contains(string(out), "node.hostname") {
		t.Fatalf("sso must not be pinned to a hostname — it has no data to keep there:\n%s", out)
	}
	if !strings.Contains(string(out), "node.role == manager") {
		t.Fatalf("sso should stay on managers:\n%s", out)
	}
}

// TestPlatformStacksFallBackToManagerRole: when nothing resolves (a single-node
// install with no storage_nodes and no worker), the stacks must still render a
// constraint rather than an empty placement.
func TestPlatformStacksFallBackToManagerRole(t *testing.T) {
	in := RenderInput{Domain: "example.com", EdgeImage: "ghcr.io/hazemarian/pmcluster-edge:v0.2.87"}
	for _, stack := range []stackName{StackObservability, StackEdge, StackBackup, StackSSO} {
		out, err := LoadComposeFile(stack, in)
		if err != nil {
			t.Fatalf("%s: %v", stack, err)
		}
		s := string(out)
		if !strings.Contains(s, "node.role ==") {
			t.Fatalf("%s rendered with no placement at all:\n%s", stack, s)
		}
		if strings.Contains(s, "node.hostname ==") {
			t.Fatalf("%s pinned to a hostname it was not given:\n%s", stack, s)
		}
	}
}
