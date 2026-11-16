package config

import (
	"strings"
	"testing"
)

// TestResolveOTLPEndpointFollowsTheCollector pins the rule: every node exports, and the
// only difference between nodes is where the one collector is.
//
// The previous rule disabled the exporter everywhere but the leader, which is how four
// daemons of five came to export nothing and the console showed a single node with no way
// to tell "quiet" from "not collected".
func TestResolveOTLPEndpointFollowsTheCollector(t *testing.T) {
	// Explicit endpoints are the operator's decision, on any node.
	if ep, _ := ResolveOTLPEndpoint("http://collector.example:4318", false, "http://10.7.224.11:4318"); ep != "http://collector.example:4318" {
		t.Fatalf("explicit endpoint on a follower = %q, want it used verbatim, overriding the leader", ep)
	}
	// Disabled stays disabled.
	if ep, reason := ResolveOTLPEndpoint("", true, ""); ep != "" || reason != "" {
		t.Fatalf("empty endpoint = (%q, %q), want disabled with no reason", ep, reason)
	}
	// auto on the leader: loopback, and the leader endpoint is not even consulted.
	ep, reason := ResolveOTLPEndpoint(OTLPAutoEndpoint, true, "http://10.7.224.12:4318")
	if ep != "http://127.0.0.1:4318" || reason != "" {
		t.Fatalf("leader auto = (%q, %q), want loopback with no reason", ep, reason)
	}
	// auto on a follower that found the collector: the leader's address.
	ep, reason = ResolveOTLPEndpoint(OTLPAutoEndpoint, false, "http://10.7.224.11:4318")
	if ep != "http://10.7.224.11:4318" || reason != "" {
		t.Fatalf("follower auto = (%q, %q), want the leader's collector", ep, reason)
	}
	// auto on a follower that has not found one: no endpoint, and a reason that says why
	// and that it is retried — never a blind dial.
	ep, reason = ResolveOTLPEndpoint(OTLPAutoEndpoint, false, "")
	if ep != "" {
		t.Fatalf("follower auto with no collector found = %q, want disabled until a probe answers", ep)
	}
	if !strings.Contains(reason, "re-probed") {
		t.Fatalf("follower reason = %q, want it to say the probe repeats", reason)
	}
}
