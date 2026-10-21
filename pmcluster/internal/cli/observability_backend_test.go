package cli

import (
	"context"
	"strconv"
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

// fakeLocator serves the swarm facts the read path resolves against.
type fakeLocator struct {
	nodes    []runtime.Node
	services []runtime.Service
	tasks    map[string][]runtime.ServiceTask
}

func (f fakeLocator) NodeList(context.Context) ([]runtime.Node, error) { return f.nodes, nil }
func (f fakeLocator) ServiceList(context.Context) ([]runtime.Service, error) {
	return f.services, nil
}
func (f fakeLocator) ServiceTasks(_ context.Context, id string) ([]runtime.ServiceTask, error) {
	return f.tasks[id], nil
}

// TestObservabilityEndpointFollowsTheRunningTask is the whole point of resolving
// from the swarm: the port is published in HOST mode, so it answers on exactly
// one node — the one running the Parseable task. A reader that trusts a
// placement SETTING instead dials an address where nothing listens the moment
// that setting is unset, renamed, or simply not what placed the task.
func TestObservabilityEndpointFollowsTheRunningTask(t *testing.T) {
	port := strconv.Itoa(ObservabilitySourcePort)
	loc := fakeLocator{
		nodes: []runtime.Node{
			{Hostname: "nxt-sw-1-m", Address: "10.7.224.12:2377"},
			{Hostname: "nxt-sw-2-m", Address: "10.7.224.13:2377"},
		},
		services: []runtime.Service{
			{ID: "svc-parseable", Name: observabilityServiceName},
			{ID: "svc-other", Name: "infra_traefik"},
		},
		tasks: map[string][]runtime.ServiceTask{
			"svc-parseable": {
				{State: "failed", Node: "nxt-sw-1-m"},
				{State: "running", Node: "nxt-sw-2-m"},
			},
			"svc-other": {{State: "running", Node: "nxt-sw-1-m"}},
		},
	}

	// The task's node is the answer.
	if got, want := observabilityEndpoint(context.Background(), loc), "http://10.7.224.13:"+port; got != want {
		t.Fatalf("endpoint = %q, want %q (the running task's node)", got, want)
	}

	// No running task, or no swarm client at all: loopback, never an
	// unroutable URL — and never a panic.
	if got, want := observabilityEndpoint(context.Background(), fakeLocator{}), "http://127.0.0.1:"+port; got != want {
		t.Fatalf("no task endpoint = %q, want %q", got, want)
	}
	if got, want := observabilityEndpoint(context.Background(), nil), "http://127.0.0.1:"+port; got != want {
		t.Fatalf("nil locator endpoint = %q, want %q", got, want)
	}
}

// TestNodeHostStripsTheManagerPort pins the shape the swarm reports: Address is
// "10.7.224.12:2377" (host + raft port) on a manager and empty on a worker.
// Using it verbatim built "[10.7.224.12:2377]:8000", which url.Parse rejects —
// the daemon then answered "telemetry unavailable" with a healthy backend.
func TestNodeHostStripsTheManagerPort(t *testing.T) {
	// The whitespace case is deliberate: a value read back from a config file can
	// arrive padded, and it must not become a URL host with a space in it.
	for in, want := range map[string]string{
		"10.7.224.12:2377": "10.7.224.12",
		"10.7.224.12":      "10.7.224.12",
		"[fd00::1]:2377":   "fd00::1",
	} {
		if got := nodeHost(in); got != want {
			t.Fatalf("nodeHost(%q) = %q, want %q", in, got, want)
		}
	}
	if got := nodeHost(" 10.7.224.13 "); got != "10.7.224.13" {
		t.Fatalf("nodeHost did not trim whitespace: %q", got)
	}
}
