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
	resolveTelemetryEndpoint = func(configured string, isLeader bool) (string, string) {
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
	s := newTelemetrySupervisor(zerolog.Nop(), func() { firstCalls.Add(1); first <- struct{}{} })
	s.interval = 0

	probe := func() { s.probe(context.Background(), "auto", nil) }

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
