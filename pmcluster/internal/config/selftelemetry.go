package config

import "strings"

// OTLPAutoEndpoint is the default value of otlp_endpoint. It does not name an address;
// it asks the process to place itself by ROLE (see ResolveOTLPEndpoint). Empty string
// still means "disabled".
const OTLPAutoEndpoint = "auto"

// OTLPPort is the collector's OTLP/HTTP receiver port, published by the observability
// stack with `mode: host` — it exists on the node running the collector, which is the
// leader, and nowhere else (internal/cluster/embeds/observability-stack.yml).
const OTLPPort = 4318

// loopbackAddress is where the leader's daemon reaches its own collector.
const loopbackAddress = "127.0.0.1"

// ResolveOTLPEndpoint turns the configured otlp_endpoint value into the base URL this
// daemon's exporters dial.
//
//   - ""               → disabled: the operator turned it off.
//   - "auto" (default) → loopback on the LEADER, disabled everywhere else.
//   - anything else    → used verbatim: an explicit endpoint is the operator's decision.
//
// The rule is one line of topology. The collector follows the leader by name and its port
// is published there only, so exactly one daemon has a collector beside it. That daemon
// dials loopback; every other daemon has nothing to dial and says so once, because it
// reports its numbers to the leader over swarm mTLS instead of exporting them itself.
//
// This replaced a per-node answer found by dialling the node's own swarm address. That
// machinery existed because the port was ingress-published — bound on every node — so
// "does anything answer here" was a real question. It is now a property of the role, and
// a dial can only ever rediscover what the code already knows. Worse, on a worker the
// probe succeeded in finding nothing and logged it as a FAULT once a minute, which reads
// as a broken node rather than as the design.
//
// The history is worth keeping, because it explains why loopback needed care. Pointing
// every daemon at 127.0.0.1 was once catastrophic: on 2026-11-08 nxt-sw-4-m (1 vCPU)
// wrote 261,617 journal lines from a single boot. Unbounded OTel export errors against a
// closed port starved journald, Docker's heartbeat slipped, the swarm marked the node
// Down and leadership moved. What made loopback unsafe was never loopback itself — it was
// every node dialling it. Only the leader does now, and the SDK's error handler is bounded
// (internal/telemetry/failures.go), so a dead collector costs one line instead of a node.
func ResolveOTLPEndpoint(configured string, isLeader bool) (endpoint, disabledReason string) {
	switch v := strings.TrimSpace(configured); {
	case v == "":
		return "", ""
	case !strings.EqualFold(v, OTLPAutoEndpoint):
		return v, ""
	}
	if !isLeader {
		return "", "this node runs no collector (it follows the leader): reporting to the leader over mTLS instead of exporting here"
	}
	return "http://" + loopbackAddress + ":" + portString(OTLPPort), ""
}

// portString formats the OTLP port without pulling strconv into a file this small.
func portString(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [6]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
