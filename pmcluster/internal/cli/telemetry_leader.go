package cli

// Finding the collector from a node that does not run it.
//
// There is exactly one collector and it follows the leader, so a follower's telemetry
// endpoint is "wherever the leader is right now". A manager can read the swarm and see
// which node is the leader; a WORKER cannot (node list and node inspect are manager-only),
// and the swarm API never reports the leader to a worker at all.
//
// So this asks the same question the mTLS assignment channel asks, in the same way: take
// every manager address the swarm knows about and use the first that ANSWERS. Only the
// leader has the collector published, so the first answer IS the leader — no leader
// election to duplicate, nothing to keep in sync, and the answer corrects itself the next
// time it is asked (once a minute, from telemetrySupervisor).
//
// The dial is what makes this safe to hand an exporter. Handing an exporter an address
// nobody listens on is what wrote 261,617 journal lines on nxt-sw-4-m in one boot
// (2026-11-08): the SDK's error handler is bounded now, but the cheapest fix is still not
// to point it at a closed port.

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
)

// collectorDialTimeout bounds one probe. It runs inside the supervisor's minute, so a
// manager that is up but not answering costs 700 ms, not the whole probe.
const collectorDialTimeout = 700 * time.Millisecond

// collectorEndpointFor returns the base URL of the first manager that answers on the
// collector's OTLP port, or "" when none does.
//
// hosts is the swarm's manager addresses (managerHosts); dial is injected so a test can
// answer without binding a port.
func collectorEndpointFor(ctx context.Context, hosts func(context.Context) ([]string, error), dial func(context.Context, string) bool) string {
	if hosts == nil || dial == nil {
		return ""
	}
	addrs, err := hosts(ctx)
	if err != nil {
		return ""
	}
	seen := make(map[string]bool, len(addrs))
	for _, host := range addrs {
		host = trimHostPort(host)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		target := net.JoinHostPort(host, strconv.Itoa(config.OTLPPort))
		if dial(ctx, target) {
			return "http://" + target
		}
	}
	return ""
}

// dialCollector is the production dial: a TCP connect with a short timeout. It does not
// speak OTLP — a port that accepts a connection is the same evidence the exporters need,
// and anything more would be a second implementation of the collector's health.
func dialCollector(ctx context.Context, target string) bool {
	d := net.Dialer{Timeout: collectorDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// trimHostPort drops a port from a swarm address ("10.7.224.11:2377" → "10.7.224.11") so a
// manager address taken from RemoteManagers can be re-addressed on the collector's port.
func trimHostPort(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
