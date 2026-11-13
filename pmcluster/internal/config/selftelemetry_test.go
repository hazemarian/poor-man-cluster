package config

import (
	"strings"
	"testing"
)

// TestResolveOTLPEndpointIsARoleNotAMeasurement pins the rule that replaced a live dial.
//
// The collector follows the leader and its port is published there only, so exactly one
// daemon has one beside it. A probe could only ever rediscover that, and on every worker
// it turned "you have no collector, by design" into a connection-refused warning once a
// minute — a fault message for the intended state.
func TestResolveOTLPEndpointIsARoleNotAMeasurement(t *testing.T) {
	// Explicit endpoints are the operator's decision, on any node.
	if ep, _ := ResolveOTLPEndpoint("http://collector.example:4318", false); ep != "http://collector.example:4318" {
		t.Fatalf("explicit endpoint on a worker = %q, want it used verbatim", ep)
	}
	// Disabled stays disabled.
	if ep, reason := ResolveOTLPEndpoint("", true); ep != "" || reason != "" {
		t.Fatalf("empty endpoint = (%q, %q), want disabled with no reason", ep, reason)
	}
	// auto: loopback on the leader.
	ep, reason := ResolveOTLPEndpoint(OTLPAutoEndpoint, true)
	if ep != "http://127.0.0.1:4318" || reason != "" {
		t.Fatalf("leader auto = (%q, %q), want loopback with no reason", ep, reason)
	}
	// auto everywhere else: no endpoint, and a reason that states the design.
	ep, reason = ResolveOTLPEndpoint(OTLPAutoEndpoint, false)
	if ep != "" {
		t.Fatalf("worker auto = %q, want no endpoint (it reports to the leader instead)", ep)
	}
	if !strings.Contains(reason, "reports") && !strings.Contains(reason, "reporting") {
		t.Fatalf("worker reason = %q, want it to say the node reports to the leader", reason)
	}
}
