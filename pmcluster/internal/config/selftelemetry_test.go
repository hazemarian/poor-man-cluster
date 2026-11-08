package config

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// stubAddr is a SwarmAddrFunc returning a fixed answer.
func stubAddr(addr string, err error) SwarmAddrFunc {
	return func(context.Context) (string, error) { return addr, err }
}

// okDial records the address it was asked about and reports success.
func okDial(dialed *string) DialFunc {
	return func(_ context.Context, addr string) error {
		if dialed != nil {
			*dialed = addr
		}
		return nil
	}
}

// TestResolveOTLPEndpoint_UsesSwarmAddress is the core contract: the default
// endpoint is THIS node's swarm address with the collector port, and the dial
// check is asked about exactly that address.
func TestResolveOTLPEndpoint_UsesSwarmAddress(t *testing.T) {
	var dialed string
	ep, reason := ResolveOTLPEndpoint(context.Background(), OTLPAutoEndpoint,
		stubAddr("10.7.224.15", nil), okDial(&dialed))

	if reason != "" {
		t.Fatalf("expected telemetry enabled, got disabled reason %q", reason)
	}
	if want := "http://10.7.224.15:4318"; ep != want {
		t.Errorf("endpoint = %q, want %q", ep, want)
	}
	if dialed != "10.7.224.15:4318" {
		t.Errorf("dial-check target = %q, want 10.7.224.15:4318", dialed)
	}
}

// TestResolveOTLPEndpoint_EmptyDisables pins the documented way to turn
// self-telemetry off: an empty otlp_endpoint, no reason line, no resolver calls.
func TestResolveOTLPEndpoint_EmptyDisables(t *testing.T) {
	called := false
	ep, reason := ResolveOTLPEndpoint(context.Background(), "",
		func(context.Context) (string, error) { called = true; return "10.7.224.15", nil },
		func(context.Context, string) error { called = true; return nil })

	if ep != "" || reason != "" {
		t.Errorf("endpoint/reason = %q/%q, want \"\"/\"\" (silently disabled)", ep, reason)
	}
	if called {
		t.Error("an empty endpoint must not read the swarm or dial anything")
	}
}

// TestResolveOTLPEndpoint_ExplicitConfigWins: an operator-supplied endpoint is
// used verbatim. The resolver must not override it, re-resolve it or dial it —
// pointing at something unusual is the operator's call, and a startup check that
// silently disabled it would be a worse surprise than a quiet exporter.
func TestResolveOTLPEndpoint_ExplicitConfigWins(t *testing.T) {
	swarmCalled, dialCalled := false, false
	ep, reason := ResolveOTLPEndpoint(context.Background(), "http://127.0.0.1:4318",
		func(context.Context) (string, error) { swarmCalled = true; return "10.7.224.15", nil },
		func(context.Context, string) error { dialCalled = true; return errors.New("refused") })

	if ep != "http://127.0.0.1:4318" {
		t.Errorf("endpoint = %q, want the configured value verbatim", ep)
	}
	if reason != "" {
		t.Errorf("explicit config must not disable telemetry, reason = %q", reason)
	}
	if swarmCalled || dialCalled {
		t.Errorf("explicit config must short-circuit resolution (swarm called=%v, dial called=%v)", swarmCalled, dialCalled)
	}
}

// TestResolveOTLPEndpoint_NoSwarmAddressDisablesEvenWithDefaultRoute is the
// guard against the tempting wrong fix. This test machine HAS a default route
// and a private address on it; the resolver must still refuse to invent an
// endpoint from the host's networking and must disable telemetry instead.
func TestResolveOTLPEndpoint_NoSwarmAddressDisablesEvenWithDefaultRoute(t *testing.T) {
	if local := routeDerivedLocalAddr(t); local != "" {
		t.Logf("this host has a route-derived private address %s — it must not be used", local)
	}

	ep, reason := ResolveOTLPEndpoint(context.Background(), OTLPAutoEndpoint,
		stubAddr("", errors.New("this node is not part of a swarm")), okDial(nil))

	if ep != "" {
		t.Fatalf("endpoint = %q, want disabled: no swarm address means no telemetry", ep)
	}
	if !strings.Contains(reason, "swarm node address unavailable") {
		t.Errorf("reason = %q, want it to name the swarm lookup", reason)
	}
	if strings.Contains(reason, "127.0.0.1") || strings.Contains(ep, "127.0.0.1") {
		t.Error("resolution fell back to loopback")
	}
}

// TestResolveOTLPEndpoint_UnreachableCollectorDisables: the address can be
// right while nothing listens (stack not deployed yet, collector restarting).
// Exporting to it would be the 2026-11-08 storm with a different address in it.
func TestResolveOTLPEndpoint_UnreachableCollectorDisables(t *testing.T) {
	ep, reason := ResolveOTLPEndpoint(context.Background(), OTLPAutoEndpoint,
		stubAddr("10.7.224.15", nil),
		func(context.Context, string) error { return errors.New("connect: connection refused") })

	if ep != "" {
		t.Fatalf("endpoint = %q, want disabled when nothing answers", ep)
	}
	if !strings.Contains(reason, "nothing answers at http://10.7.224.15:4318") {
		t.Errorf("reason = %q, want it to name the probed address", reason)
	}
}

// TestResolveOTLPEndpoint_RejectsUnusableSwarmAddresses: only an address a peer
// could dial is acceptable. Loopback here would recreate the exact bug (a
// daemon exporting to a port that only exists on one node).
func TestResolveOTLPEndpoint_RejectsUnusableSwarmAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "::1", "0.0.0.0", "not-an-ip", " "} {
		ep, reason := ResolveOTLPEndpoint(context.Background(), OTLPAutoEndpoint,
			stubAddr(addr, nil), okDial(nil))
		if ep != "" {
			t.Errorf("addr %q: endpoint = %q, want disabled", addr, ep)
		}
		if reason == "" {
			t.Errorf("addr %q: disabled without a reason", addr)
		}
	}
}

// TestResolveOTLPEndpoint_NeverServiceName pins the second wrong fix: the
// overlay's DNS name. A host process cannot resolve it, so it must never be
// produced from the default path.
func TestResolveOTLPEndpoint_NeverServiceName(t *testing.T) {
	ep, _ := ResolveOTLPEndpoint(context.Background(), OTLPAutoEndpoint,
		stubAddr("10.7.224.15", nil), okDial(nil))
	if ep == "" {
		t.Fatal("expected an endpoint for a reachable swarm address")
	}
	if strings.Contains(ep, "observability_otel-collector") || strings.Contains(ep, "localhost") {
		t.Errorf("endpoint = %q, must name the node address, never the overlay service", ep)
	}
}

// TestLoad_DefaultEndpointIsAuto: the shipped default is the resolver sentinel,
// not a host. A regression to any literal address re-creates the incident on
// every node that is not the one the literal points at.
func TestLoad_DefaultEndpointIsAuto(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OTLPEndpoint != OTLPAutoEndpoint {
		t.Errorf("default otlp_endpoint = %q, want %q", cfg.OTLPEndpoint, OTLPAutoEndpoint)
	}
}

// TestLoad_EmptyEnvDisables: viper drops empty env values, so PMCLUSTER_OTLP_
// ENDPOINT= would otherwise fall back to the "auto" default. Empty must mean
// disabled everywhere it is spelled.
func TestLoad_EmptyEnvDisables(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PMCLUSTER_OTLP_ENDPOINT", "")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OTLPEndpoint != "" {
		t.Errorf("otlp_endpoint = %q, want empty (disabled)", cfg.OTLPEndpoint)
	}
}

// routeDerivedLocalAddr reports the private address a default-route dial would
// yield, purely so the test above can state that such an address exists. It is
// test scaffolding: the resolver itself has no such path.
func routeDerivedLocalAddr(t *testing.T) string {
	t.Helper()
	c, err := net.Dial("udp", "198.51.100.1:9") // TEST-NET-2, no packet is sent
	if err != nil {
		return ""
	}
	defer func() { _ = c.Close() }()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || ua.IP.IsLoopback() || ua.IP.IsUnspecified() {
		return ""
	}
	return ua.IP.String()
}
