package cluster

import (
	"testing"

	"sigs.k8s.io/yaml"
)

// stackPort is one entry of a rendered service's ports list.
type stackPort struct {
	Target    int    `json:"target"`
	Published int    `json:"published"`
	Protocol  string `json:"protocol"`
	Mode      string `json:"mode"`
}

// TestObservabilityOTLPIsPublishedOnTheMesh pins the publish mode of the
// collector's OTLP/HTTP port in the RENDERED stack, not in the template text.
//
// The daemon is a host process that dials this node's swarm address on 4318. In
// host mode that port exists only on the node running the collector task, so
// every other node's daemon exported into a closed port — the 2026-11-08 journal
// storm that took nxt-sw-4-m out of the swarm. Ingress makes the routing mesh
// bind 4318 on every node from a single task, which is the only shape where a
// daemon's endpoint does not depend on where the task happens to land.
func TestObservabilityOTLPIsPublishedOnTheMesh(t *testing.T) {
	body, err := LoadComposeFile(StackObservability, RenderInput{
		Domain:                     "example.com",
		ObservabilityAdminEmail:    "admin@example.com",
		ObservabilityAdminPassword: "not-a-real-password",
	})
	if err != nil {
		t.Fatalf("render observability stack: %v", err)
	}

	var doc struct {
		Services map[string]struct {
			Ports []stackPort `json:"ports"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("rendered observability stack is not valid YAML: %v", err)
	}

	svc, ok := doc.Services["otel-collector"]
	if !ok {
		names := make([]string, 0, len(doc.Services))
		for k := range doc.Services {
			names = append(names, k)
		}
		t.Fatalf("rendered stack has no otel-collector service (services: %v)", names)
	}
	if len(svc.Ports) != 1 {
		t.Fatalf("otel-collector publishes %d ports, want exactly 1 (the OTLP/HTTP receiver): %+v", len(svc.Ports), svc.Ports)
	}
	p := svc.Ports[0]
	if p.Target != 4318 || p.Published != 4318 {
		t.Errorf("published port = %d->%d, want 4318->4318", p.Published, p.Target)
	}
	if p.Mode != "ingress" {
		t.Errorf("OTLP publish mode = %q, want \"ingress\": host mode binds 4318 only where the task runs, which is the closed port every other daemon used to export to", p.Mode)
	}
	if p.Protocol != "tcp" {
		t.Errorf("OTLP publish protocol = %q, want tcp", p.Protocol)
	}
}
