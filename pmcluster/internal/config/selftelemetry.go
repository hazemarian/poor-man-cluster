package config

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// OTLPAutoEndpoint is the default value of otlp_endpoint. It does not name an
// address; it asks the process to resolve THIS node's own address at startup
// (see ResolveOTLPEndpoint). Empty string still means "disabled".
const OTLPAutoEndpoint = "auto"

// OTLPPort is the collector's OTLP/HTTP receiver port, published by the
// observability stack with `mode: ingress` so every node in the swarm answers
// on it (internal/cluster/embeds/observability-stack.yml).
const OTLPPort = 4318

// selfTelemetryDialTimeout bounds the startup reachability check. It runs once,
// before the daemon serves, so a dead endpoint must not delay startup by more
// than a few seconds.
const selfTelemetryDialTimeout = 2 * time.Second

// SwarmAddrFunc reports this node's swarm-advertised address (node.Status.Addr).
// The daemon passes internal/docker.NodeSelfAddr; tests pass a stub.
type SwarmAddrFunc func(ctx context.Context) (string, error)

// DialFunc reports whether addr accepts a TCP connection. nil means the real
// TCP dial with selfTelemetryDialTimeout.
type DialFunc func(ctx context.Context, addr string) error

// ResolveOTLPEndpoint turns the configured otlp_endpoint value into the base
// URL the OTel exporters dial.
//
//   - ""              → disabled ("" and no reason): the operator turned it off.
//   - "auto" (default)→ this node's swarm address, dial-checked; disabled with
//     a reason when the address cannot be read or nothing answers there.
//   - anything else   → used verbatim. An explicit endpoint is the operator's
//     decision and is never second-guessed.
//
// Why the address comes from the swarm and from nowhere else.
//
// The daemon is a systemd process on the host, outside the swarm overlay. It
// cannot resolve `observability_otel-collector` — that name only exists inside
// the overlay's DNS — so pointing OTLPEndpoint at the service name (the obvious
// fix, and the way every application on this cluster is wired) fails on every
// node with a DNS error instead of a refused connection.
//
// 127.0.0.1 was the constant this used to be, and it is only correct on the one
// node that happens to be running the collector task. Everywhere else it is a
// closed port, and an exporter pointed at a closed port is not a harmless
// no-op: the OTel SDK's default error handler logs every failed export, with no
// rate limit and no recovery path. On 2026-11-08 that cost node nxt-sw-4-m
// (1 vCPU) 261,617 journald lines from a single daemon boot plus 238,951 /
// 213,808 / 196,367 / 151,609 / 71,180 from other incarnations; journald writes
// starved Docker's heartbeat, the swarm marked the node Down, and leadership
// moved. A missing trace is acceptable. A wrong endpoint is not.
//
// Not route-derived either: neither the default-route interface, nor a UDP
// "connect" trick, nor `hostname -I` names the address the swarm knows this
// node by. Each of those can just as easily produce a private address that
// nothing listens on — the same closed port, the same storm, reached by another
// route. So the only accepted source is the swarm's own record of this node,
// and the only accepted answer is one that answers a dial.
func ResolveOTLPEndpoint(ctx context.Context, configured string, swarmAddr SwarmAddrFunc, dial DialFunc) (endpoint, disabledReason string) {
	switch v := strings.TrimSpace(configured); {
	case v == "":
		return "", ""
	case !strings.EqualFold(v, OTLPAutoEndpoint):
		return v, ""
	}

	if swarmAddr == nil {
		return "", "self-telemetry disabled: no swarm address source is wired in"
	}
	addr, err := swarmAddr(ctx)
	if err != nil {
		return "", fmt.Sprintf("self-telemetry disabled: swarm node address unavailable: %v", err)
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "self-telemetry disabled: the swarm reported an empty address for this node"
	}
	ip, perr := netip.ParseAddr(addr)
	if perr != nil {
		return "", fmt.Sprintf("self-telemetry disabled: swarm node address %q is not an IP address", addr)
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
		return "", fmt.Sprintf("self-telemetry disabled: swarm node address %q is not routable from a peer", addr)
	}

	target := net.JoinHostPort(addr, strconv.Itoa(OTLPPort))
	if dial == nil {
		dial = tcpDial
	}
	if err := dial(ctx, target); err != nil {
		return "", fmt.Sprintf("self-telemetry disabled: nothing answers at http://%s (%v)", target, err)
	}
	return "http://" + target, ""
}

// tcpDial is the default DialFunc: one TCP connect, bounded.
func tcpDial(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: selfTelemetryDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}
