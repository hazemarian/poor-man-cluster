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

// TestObservabilityOTLPIsPublishedOnTheLeaderOnly pins the publish mode of the
// collector's OTLP/HTTP port in the RENDERED stack, not in the template text.
//
// The mode flipped back to `host` — and it is the guard around it that had to change,
// not the mode. Host mode shipped once with no daemon-side guard: every non-leader daemon
// dialled a port nothing bound locally, the OTel SDK logged each failure, and on
// 2026-11-08 one node wrote 261,617 journal lines from a single boot until journald
// starved docker's heartbeat and the swarm marked it Down. Ingress "fixed" it by binding
// 4318 on every node and forwarding to the one task.
//
// The flood is now fixed at the source — telemetry.SetErrorHandler is bounded and the
// supervisor stops an exporter whose endpoint stops answering — so the port goes back to
// the narrow shape: host mode, on the leader, where the daemon that dials it lives.
// If this test ever fails in the other direction, check the error handler first.
func TestObservabilityOTLPIsPublishedOnTheLeaderOnly(t *testing.T) {
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
	if p.Mode != "host" {
		t.Errorf("OTLP publish mode = %q, want \"host\": ingress binds 4318 on EVERY node and forwards to one task, which is exposure nobody needs now that nodes report over mTLS", p.Mode)
	}
	if p.Protocol != "tcp" {
		t.Errorf("OTLP publish protocol = %q, want tcp", p.Protocol)
	}
}
