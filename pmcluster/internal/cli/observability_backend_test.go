package cli

import (
	"context"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
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
			src, err := localObservabilitySource(tc.user, tc.pass)
			if err == nil {
				t.Fatalf("want an error for %s, got a client (%v)", name, src)
			}
			if src != nil {
				t.Fatal("want a nil client when the credential is unusable")
			}
		})
	}

	src, err := localObservabilitySource("admin@example.test", "secret")
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
