package cli

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// TestObservabilitySourcePortMatchesCluster keeps the CLI's dial target tied to
// the constant the stack template publishes. They are two views of one fact: if
// they drift, every telemetry read fails with "source unavailable" and nothing
// says why.
func TestObservabilitySourcePortMatchesCluster(t *testing.T) {
	if ObservabilitySourcePort != cluster.ObservabilityPort {
		t.Fatalf("ObservabilitySourcePort = %d, cluster.ObservabilityPort = %d", ObservabilitySourcePort, cluster.ObservabilityPort)
	}
	if ObservabilitySourcePort <= 0 || ObservabilitySourcePort > 65535 {
		t.Fatalf("ObservabilitySourcePort = %d is not a usable port", ObservabilitySourcePort)
	}
}

// TestLocalObservabilitySourceValidatesCredential proves a half-configured
// credential fails at construction with a specific error, rather than building
// an adapter that 401s on every read and reports "source unavailable".
func TestLocalObservabilitySourceValidatesCredential(t *testing.T) {
	for name, tc := range map[string]struct{ user, pass string }{
		"empty password": {"admin@example.test", "  "},
		"empty username": {"", "secret"},
		"both empty":     {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			src, err := localObservabilitySource("http://127.0.0.1:8000", tc.user, tc.pass)
			if err == nil {
				t.Fatalf("want an error for %s, got a client (%v)", name, src)
			}
			if src != nil {
				t.Fatal("want a nil client when the credential is unusable")
			}
		})
	}

	src, err := localObservabilitySource("http://127.0.0.1:8000", "admin@example.test", "secret")
	if err != nil {
		t.Fatalf("valid credential rejected: %v", err)
	}
	if src == nil || src.Stream() == "" {
		t.Fatal("valid credential produced no usable source")
	}
}

// TestLocalObservabilityReaderDegradesToInventory is the daemon's startup
// guarantee: no store, no cipher, or a missing credential must leave a reader
// that still reports Swarm inventory, so the console can show node health while
// every telemetry read answers "unavailable" instead of the daemon refusing to
// start.
func TestLocalObservabilityReaderDegradesToInventory(t *testing.T) {
	reader, err := newLocalObservabilityReader(context.Background(), nil, nil, nil)
	if err == nil {
		t.Fatal("want a diagnostic error so the daemon can log why telemetry is off")
	}
	if reader == nil {
		t.Fatal("want a usable reader even when the source is unavailable")
	}
	w, werr := observability.NewWindow(time.Time{}, time.Hour)
	if werr != nil {
		t.Fatalf("build window: %v", werr)
	}
	if _, statusErr := reader.Status(context.Background(), w); statusErr != nil {
		t.Fatalf("inventory-only status must not fail: %v", statusErr)
	}
	if _, logsErr := reader.Logs(context.Background(), observability.LogQuery{Window: w, Limit: 10}); logsErr == nil {
		t.Fatal("want ErrUnavailable from a reader with no telemetry source")
	}
}

// TestObservabilityEndpointFollowsThePlatformNode pins where the daemon reads.
//
// The port is published in HOST mode, so exactly one node binds it — the one
// running the Parseable task. A daemon that dials loopback on every node reads
// its own machine, where nothing listens, and reports "telemetry unavailable"
// while the backend is healthy. So the address must come from the swarm.
func TestObservabilityEndpointFollowsThePlatformNode(t *testing.T) {
	// Address carries the MANAGER address, port and all ("10.7.224.12:2377").
	// That is what the swarm reports, and using it verbatim builds a URL the
	// Parseable client rejects — the whole read path then reports "unavailable"
	// with a healthy backend.
	nodes := []runtime.Node{
		{Hostname: "nxt-sw-1-m", Address: "10.7.224.12:2377"},
		{Hostname: "nxt-sw-2-m", Address: "10.7.224.13:2377"},
	}
	got := observabilityEndpoint("nxt-sw-2-m", nodes)
	if want := "http://10.7.224.13:" + strconv.Itoa(ObservabilitySourcePort); got != want {
		t.Fatalf("endpoint = %q, want %q", got, want)
	}

	// A bare address (a worker reports none, but a future source might) must work
	// the same way.
	if got := observabilityEndpoint("nxt-sw-1-m", []runtime.Node{{Hostname: "nxt-sw-1-m", Address: "10.7.224.12"}}); got != "http://10.7.224.12:"+strconv.Itoa(ObservabilitySourcePort) {
		t.Fatalf("bare address endpoint = %q", got)
	}

	// Unknown platform node (a rename, or a node list that came back empty)
	// degrades to loopback instead of building an unroutable address.
	if got := observabilityEndpoint("gone-node", nodes); !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Fatalf("unknown platform node should fall back to loopback, got %q", got)
	}
	if got := observabilityEndpoint("", nil); !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Fatalf("no platform node should fall back to loopback, got %q", got)
	}

	// A node with no address must not produce "http://:8000".
	if got := observabilityEndpoint("nxt-sw-1-m", []runtime.Node{{Hostname: "nxt-sw-1-m"}}); !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Fatalf("addressless node should fall back to loopback, got %q", got)
	}
}
