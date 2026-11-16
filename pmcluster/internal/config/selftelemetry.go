package config

import "strings"

// OTLPAutoEndpoint is the default value of otlp_endpoint. It does not name an address;
// it asks the process to place itself by ROLE (see ResolveOTLPEndpoint). Empty string
// still means "disabled".
const OTLPAutoEndpoint = "auto"

// OTLPPort is the collector's OTLP/HTTP receiver port. There is ONE collector, on the
// leader, published with `mode: host` (internal/cluster/embeds/observability-stack.yml),
// so the port exists on the leader and nowhere else.
const OTLPPort = 4318

// loopbackAddress is where the leader's daemon reaches the collector running beside it.
const loopbackAddress = "127.0.0.1"

// ResolveOTLPEndpoint turns the configured otlp_endpoint value into the base URL this
// daemon's exporters dial.
//
//   - ""               → disabled: the operator turned it off.
//   - "auto" (default) → the collector, wherever it is: loopback on the leader, the
//     leader's own address on every other node.
//   - anything else    → used verbatim: an explicit endpoint is the operator's decision.
//
// Every node exports its own logs, metrics and traces, and they all land in the one
// collector the platform runs. The difference between nodes is only WHERE that collector
// is: beside the leader, so the leader dials loopback and everyone else dials the leader.
//
// This replaced "loopback on the leader, DISABLED everywhere else". Disabled was not a
// neutral choice: it meant four daemons of five exported nothing, the node reports that
// carried their numbers landed in the leader's SQLite and stopped there, and the console
// showed one node in a five-node cluster with no way to tell "quiet" from "not collected".
// The telemetry never had to stop at the leader — it had to reach the collector, and the
// collector is reachable from every node over the swarm data path.
//
// leaderEndpoint is only consulted on a non-leader, and is expected to be the empty string
// unless something has ALREADY answered on the collector port (the caller dials the swarm
// manager addresses first). That ordering is deliberate: pointing an exporter at a port
// nobody listens on is what saturated nxt-sw-4-m's journal with 261,617 lines from a single
// boot on 2026-11-08, and the SDK's bounded error handler is a safety net, not a licence to
// dial blind.
func ResolveOTLPEndpoint(configured string, isLeader bool, leaderEndpoint string) (endpoint, disabledReason string) {
	switch v := strings.TrimSpace(configured); {
	case v == "":
		return "", ""
	case !strings.EqualFold(v, OTLPAutoEndpoint):
		return v, ""
	}
	if isLeader {
		return "http://" + loopbackAddress + ":" + portString(OTLPPort), ""
	}
	if e := strings.TrimSpace(leaderEndpoint); e != "" {
		return e, ""
	}
	return "", "no manager is answering the collector's OTLP port yet (the collector runs on the leader); re-probed every minute"
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
