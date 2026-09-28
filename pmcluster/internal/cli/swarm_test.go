package cli

import (
	"strings"
	"testing"
)

// TestDetectNodeIPReturnsAddress verifies detectNodeIP finds a plausible
// non-loopback IPv4 on the test host (or degrades to empty on loopback-only
// sandboxes — never a hard failure).
func TestDetectNodeIPReturnsAddress(t *testing.T) {
	ip := detectNodeIP()
	if ip == "" {
		t.Log("no non-loopback IPv4 on this host — acceptable in sandboxes")
		return
	}
	if strings.ContainsAny(ip, " \t\n") {
		t.Fatalf("detectNodeIP returned a malformed address %q", ip)
	}
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		t.Fatalf("detectNodeIP returned %q, expected a dotted IPv4", ip)
	}
}

// TestSwarmActiveNeverErrors verifies swarmActive is a safe no-panic probe
// even when the Docker daemon is unreachable (returns false, never errors).
func TestSwarmActiveNeverErrors(t *testing.T) {
	_ = swarmActive(t.Context()) // must not panic or hang
}