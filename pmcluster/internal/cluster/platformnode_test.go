package cluster

import (
	"strings"
	"testing"
)

func TestPlatformNodePinsAllStacks(t *testing.T) {
	for _, name := range []stackName{StackInfra, StackObservability, StackEdge, StackBackup, StackSSO} {
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

func TestPlatformNodeEmptyKeepsManagerRole(t *testing.T) {
	in := RenderInput{Domain: "example.com", EdgeImage: "ghcr.io/hazemarian/pmcluster-edge:v0.2.87"}
	out, err := LoadComposeFile(StackInfra, in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "node.role == manager") {
		t.Errorf("default should keep manager role:\n%s", out)
	}
}
