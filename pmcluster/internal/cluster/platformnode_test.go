package cluster

import (
	"strings"
	"testing"
)

// TestPlatformNodePinsNonIngressStacks: infra (traefik) is the ingress
// gateway and is pinned to MANAGER nodes (its swarm provider needs manager
// access), so it is excluded here. The remaining platform stacks pin to the
// designated platform node (node.hostname) when set.
func TestPlatformNodePinsAllStacks(t *testing.T) {
	for _, name := range []stackName{StackObservability, StackEdge, StackBackup, StackSSO} {
		in := RenderInput{Domain: "example.com", PlatformNode: "node-01", EdgeImage: "ghcr.io/hazemarian/pmcluster-edge:v0.2.87"}
		out, err := LoadComposeFile(name, in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s := string(out)
		if !strings.Contains(s, "node.hostname == node-01") {
			t.Errorf("%s: missing node.hostname pin:\n%s", name, s)
		}
		if strings.Contains(s, "node.role == manager") {
			t.Errorf("%s: stale role constraint present:\n%s", name, s)
		}
	}
}

// TestTraefikRunsOnManagers: traefik is global but constrained to manager
// nodes — with or without PlatformNode — because its swarm provider can only
// list services through a manager's Docker socket. A placement-free global
// traefik would also run on workers (provider fails there, no routers) and the
// ingress routing mesh would intermittently route requests to a routerless
// replica (prod SSO 404s).
func TestTraefikRunsOnManagers(t *testing.T) {
	for _, tc := range []struct {
		name         string
		platformNode string
	}{
		{name: "platform-node set", platformNode: "node-01"},
		{name: "no platform node", platformNode: ""},
	} {
		in := RenderInput{Domain: "example.com", PlatformNode: tc.platformNode, EdgeImage: "ghcr.io/hazemarian/pmcluster-edge:v0.2.87"}
		out, err := LoadComposeFile(StackInfra, in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		s := string(out)
		if !strings.Contains(s, "mode: global") {
			t.Errorf("%s: traefik not global:\n%s", tc.name, s)
		}
		if !strings.Contains(s, "node.role == manager") {
			t.Errorf("%s: traefik must be pinned to manager nodes:\n%s", tc.name, s)
		}
		if strings.Contains(s, "node.hostname") {
			t.Errorf("%s: traefik must not carry a node-hostname pin:\n%s", tc.name, s)
		}
	}
}

// TestPlatformNodeEmptyKeepsManagerRole: the non-ingress platform stacks fall
// back to node.role == manager when no platform node is configured.
func TestPlatformNodeEmptyKeepsManagerRole(t *testing.T) {
	for _, name := range []stackName{StackObservability, StackEdge, StackBackup, StackSSO} {
		in := RenderInput{Domain: "example.com", EdgeImage: "ghcr.io/hazemarian/pmcluster-edge:v0.2.87"}
		out, err := LoadComposeFile(name, in)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), "node.role == manager") {
			t.Errorf("%s default should keep manager role:\n%s", name, out)
		}
	}
}
