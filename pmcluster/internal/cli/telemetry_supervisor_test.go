package cli

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/telemetry"
)

// TestTelemetrySupervisorRecoversWithoutARestart is the regression for a live
// worker that disabled its exporter at boot (the collector was being
// rescheduled) and never reconsidered: a five-node cluster showed one row in the
// console until every daemon was restarted by hand.
func TestTelemetrySupervisorRecoversWithoutARestart(t *testing.T) {
	prevResolve, prevInit := resolveTelemetryEndpoint, initTelemetry
	t.Cleanup(func() { resolveTelemetryEndpoint, initTelemetry = prevResolve, prevInit })

	var endpoint atomic.Value
	endpoint.Store("")
	reason := ""
	resolveTelemetryEndpoint = func(configured string, isLeader bool, leaderEndpoint string) (string, string) {
		return endpoint.Load().(string), reason
	}
	var inits, shutdowns, firstCalls atomic.Int32
	initTelemetry = func(ctx context.Context, opts telemetry.Options) (telemetry.Shutdown, error) {
		inits.Add(1)
		if opts.Endpoint == "" {
			t.Fatalf("Init called with an empty endpoint")
		}
		return func(context.Context) error { shutdowns.Add(1); return nil }, nil
	}

	first := make(chan struct{}, 4)
	s := newTelemetrySupervisor(zerolog.Nop(), "nxt-sw-6-w", func() { firstCalls.Add(1); first <- struct{}{} })
	s.interval = 0

	probe := func() { s.probe(context.Background(), "auto", nil, nil) }

	// 1. The collector is away: no exporter is created, nothing is started.
	reason = "self-telemetry disabled: nothing answers at http://10.7.224.10:4318"
	probe()
	if inits.Load() != 0 || firstCalls.Load() != 0 {
		t.Fatalf("exporter started while the collector was unreachable: inits=%d first=%d", inits.Load(), firstCalls.Load())
	}

	// 2. The collector comes back: the exporter starts WITHOUT a daemon restart,
	//    and the sampler is started exactly once.
	endpoint.Store("http://10.7.224.10:4318")
	reason = ""
	probe()
	if inits.Load() != 1 {
		t.Fatalf("inits = %d, want 1 after the collector came back", inits.Load())
	}
	<-first

	// 3. A repeated identical answer must not restart the pipeline.
	probe()
	if inits.Load() != 1 || shutdowns.Load() != 0 {
		t.Fatalf("inits=%d shutdowns=%d, want no churn on an unchanged endpoint", inits.Load(), shutdowns.Load())
	}

	// 4. The endpoint goes away again: the exporter is torn down, not leaked.
	endpoint.Store("")
	reason = "self-telemetry disabled: nothing answers"
	probe()
	if shutdowns.Load() != 1 {
		t.Fatalf("shutdowns = %d, want the old exporter stopped", shutdowns.Load())
	}

	// 5. And it recovers a second time.
	endpoint.Store("http://10.7.224.10:4318")
	reason = ""
	probe()
	if inits.Load() != 2 {
		t.Fatalf("inits = %d, want a second start after recovery", inits.Load())
	}
	if firstCalls.Load() != 1 {
		t.Fatalf("firstEndpoint callback ran %d times, want exactly once", firstCalls.Load())
	}
}

// TestTelemetrySupervisorFollowsTheLeader is what a follower's exporter does when
// leadership moves: the collector moves with it, so the endpoint a node dials changes, and
// the supervisor must re-point the pipeline rather than keep exporting into a port the
// collector left. Nothing here is a restart — the same daemon, one probe apart.
func TestTelemetrySupervisorFollowsTheLeader(t *testing.T) {
	prevResolve, prevInit := resolveTelemetryEndpoint, initTelemetry
	t.Cleanup(func() { resolveTelemetryEndpoint, initTelemetry = prevResolve, prevInit })

	var gotEndpoints []string
	initTelemetry = func(ctx context.Context, opts telemetry.Options) (telemetry.Shutdown, error) {
		gotEndpoints = append(gotEndpoints, opts.Endpoint)
		return func(context.Context) error { return nil }, nil
	}

	leader := "http://10.7.224.11:4318"
	s := newTelemetrySupervisor(zerolog.Nop(), "nxt-sw-4-m", nil)
	s.interval = 0
	follower := func(context.Context) string { return leader }

	// 1. The leader is where the follower found it.
	s.probe(context.Background(), "auto", nil, follower)
	if len(gotEndpoints) != 1 || gotEndpoints[0] != "http://10.7.224.11:4318" {
		t.Fatalf("endpoints = %v, want the leader's collector", gotEndpoints)
	}

	// 2. Leadership moved: the next probe re-points the exporter at the new leader.
	leader = "http://10.7.224.12:4318"
	s.probe(context.Background(), "auto", nil, follower)
	if len(gotEndpoints) != 2 || gotEndpoints[1] != "http://10.7.224.12:4318" {
		t.Fatalf("endpoints = %v, want a second pipeline on the new leader", gotEndpoints)
	}

	// 3. This node becomes the leader: loopback beside the collector, and the follower
	//    lookup is not consulted at all.
	s.probe(context.Background(), "auto", func(context.Context) bool { return true }, func(context.Context) string {
		t.Fatal("a leader must not go looking for another node's collector")
		return ""
	})
	if len(gotEndpoints) != 3 || gotEndpoints[2] != "http://127.0.0.1:4318" {
		t.Fatalf("endpoints = %v, want loopback after promotion", gotEndpoints)
	}
}
