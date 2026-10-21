package cluster

// ObservabilityPort is the host port the observability stack publishes
// Parseable's HTTP API on, and the port the daemon and CLI dial on 127.0.0.1.
//
// It is published on the Swarm routing mesh rather than as a service name
// because the daemon is a HOST process: it cannot resolve
// `observability_parseable` from the overlay. The mesh means the port answers
// on 127.0.0.1 on every node, which is the same mechanism backup SeaweedFS uses
// for 8333.
//
// The value is duplicated as a literal in embeds/observability-stack.yml (the
// template cannot see Go constants); TestObservabilityPortMatchesStack keeps
// the two in step.
const ObservabilityPort = 8000

// ObservabilityAdminCredential is the managed credential Parseable is created
// with. One login serves the backend, the collector exporter, the operator's
// browser session and the daemon's read API.
const ObservabilityAdminCredential = "observability_admin"
