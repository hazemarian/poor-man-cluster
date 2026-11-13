package cli

// Self-telemetry that recovers on its own.
//
// The endpoint is this node's own swarm address, and it is only usable once the
// collector is answering on it. That was decided ONCE at startup: a node that
// booted while the collector was being rescheduled disabled its exporter
// permanently, and nothing re-checked — measured on a live worker, whose journal
// still said "self-telemetry disabled: nothing answers at http://10.7.224.10:4318
// (connection refused)" six hours after the same port had started answering 200.
// The console then showed ONE node in a five-node cluster, which reads as a
// broken panel rather than as four daemons that opted out at boot.
//
// Same correction as control-plane adoption (startup-only → also on leadership
// gain) and registry credentials (one-shot login → per deploy): a decision taken
// on a transient condition must be revisited.

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/telemetry"
)

// telemetryReprobeInterval is how often a node re-asks whether its collector is
// answering. A minute is cheap (one dial) and short enough that a rescheduled
// collector costs a minute of node stats rather than a restart.
const telemetryReprobeInterval = time.Minute

// Seams so the supervisor can be tested without a collector or a docker daemon.
var (
	resolveTelemetryEndpoint = config.ResolveOTLPEndpoint
	initTelemetry            = telemetry.Init
)

// telemetrySupervisor owns the exporter's lifetime: it starts it when an
// endpoint first answers, restarts it when the endpoint changes, and stops it
// when the answer goes away. onFirstEndpoint runs once, when telemetry first
// becomes available — that is where the node-stat sampler starts, because a
// sampler with no exporter would do work nobody reads.
type telemetrySupervisor struct {
	mu       sync.Mutex
	endpoint string
	shutdown telemetry.Shutdown
	started  bool
	log      zerolog.Logger
	// onFirstEndpoint is nil after it has run once.
	onFirstEndpoint func()
	interval        time.Duration
}

// newTelemetrySupervisor returns a supervisor that is not yet running.
func newTelemetrySupervisor(log zerolog.Logger, onFirstEndpoint func()) *telemetrySupervisor {
	return &telemetrySupervisor{
		log:             log.With().Str("component", "telemetry").Logger(),
		onFirstEndpoint: onFirstEndpoint,
		interval:        telemetryReprobeInterval,
	}
}

// Loop re-probes until ctx ends. It never blocks the caller: serve starts it in
// a goroutine and a failed probe is a log line, not a startup failure.
func (s *telemetrySupervisor) Loop(ctx context.Context, configured string, isLeader func(context.Context) bool) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	s.probe(ctx, configured, isLeader)
	for {
		select {
		case <-ctx.Done():
			s.stop(context.Background())
			return
		case <-t.C:
			s.probe(ctx, configured, isLeader)
		}
	}
}

// probe resolves the endpoint once and reconciles the exporter with it.
func (s *telemetrySupervisor) probe(ctx context.Context, configured string, isLeader func(context.Context) bool) {
	leader := isLeader != nil && isLeader(ctx)
	endpoint, reason := resolveTelemetryEndpoint(configured, leader)

	s.mu.Lock()
	defer s.mu.Unlock()
	if reason != "" && s.endpoint == "" && endpoint == "" {
		// Still unreachable: say it once per transition, not once per minute.
		s.log.Debug().Str("reason", reason).Msg("self-telemetry still unavailable")
		return
	}
	if endpoint == s.endpoint {
		return
	}
	// Something changed: drop the old exporter before starting the new one, so
	// two pipelines never export the same node under different addresses.
	s.stopLocked(ctx)
	s.endpoint = endpoint
	if endpoint == "" {
		if reason != "" {
			s.log.Warn().Str("reason", reason).Msg("self-telemetry disabled")
		} else {
			s.log.Warn().Msg("self-telemetry disabled: no endpoint")
		}
		return
	}
	shutdown, err := initTelemetry(ctx, telemetry.Options{
		Endpoint:       endpoint,
		ServiceName:    serviceName(),
		ServiceVersion: buildinfo.Version,
		Failures:       telemetryFailureLogger(s.log),
	})
	if err != nil {
		s.log.Warn().Err(err).Str("endpoint", endpoint).Msg("OTel telemetry disabled")
		s.endpoint = ""
		return
	}
	s.shutdown = shutdown
	s.log.Info().Str("endpoint", endpoint).Msg("OTel telemetry enabled")
	if !s.started && s.onFirstEndpoint != nil {
		s.started = true
		fn := s.onFirstEndpoint
		s.onFirstEndpoint = nil
		// Run outside the lock: the sampler touches docker and must not hold it.
		go fn()
	}
}

// stop tears the exporter down (used at shutdown).
func (s *telemetrySupervisor) stop(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked(ctx)
}

func (s *telemetrySupervisor) stopLocked(ctx context.Context) {
	if s.shutdown == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.shutdown(cctx); err != nil {
		s.log.Warn().Err(err).Msg("telemetry shutdown had issues")
	}
	s.shutdown = nil
}
