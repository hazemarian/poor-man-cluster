package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

// TestNodeResourceAttrsCoverEverySignal is the contract that makes a multi-node cluster's
// telemetry readable: the node rides the RESOURCE, so a log line, a metric and a span
// emitted by the same process agree without any call site remembering to add it.
//
// Without this, only metrics carried a node name (they add it per datapoint), and a log or
// span from a worker was unattributable — or, worse on the leader's relay, filed under the
// leader's own resource.
func TestNodeResourceAttrsCoverEverySignal(t *testing.T) {
	attrs := nodeResourceAttrs(Options{
		ServiceName:    "pmcluster-daemon",
		ServiceVersion: "v0.2.315",
		Node:           "nxt-sw-6-w",
	})
	got := map[string]string{}
	for _, kv := range attrs {
		if kv.Value.Type() == attribute.STRING {
			got[string(kv.Key)] = kv.Value.AsString()
		}
	}
	want := map[string]string{
		"service.name":        "pmcluster-daemon",
		"service.version":     "v0.2.315",
		"service.instance.id": "nxt-sw-6-w",
		"host.name":           "nxt-sw-6-w",
		"pmcluster.node.name": "nxt-sw-6-w",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q (have %v)", k, got[k], v, got)
		}
	}
}

// TestNodeResourceAttrsWithoutANode: a process that does not know its node (a one-shot CLI)
// must not emit an empty string as its instance id — an empty attribute reads as a node
// named "", and every such record groups together.
func TestNodeResourceAttrsWithoutANode(t *testing.T) {
	for _, kv := range nodeResourceAttrs(Options{ServiceName: "pmcluster-daemon"}) {
		switch kv.Key {
		case "service.instance.id", "host.name", "pmcluster.node.name":
			t.Fatalf("%s was emitted with no node configured: %q", kv.Key, kv.Value.AsString())
		}
	}
}
