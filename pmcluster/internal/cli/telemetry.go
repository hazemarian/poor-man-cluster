package cli

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/telemetry"
)

// initCLITelemetry wires OTel for one-shot CLI commands so the internal spans
// they emit (deploy, rollback, cluster up, backups) reach this node's collector
// and show up in the observability backend alongside the daemon's HTTP spans.
// Returns a shutdown func that MUST be deferred by the caller so batched spans
// flush before the process exits.
//
// The endpoint is resolved exactly like the daemon's: this node's swarm address,
// dial-checked (config.ResolveOTLPEndpoint). A CLI run off-cluster — an
// operator's laptop, a CI runner — has no swarm address, so it exports nothing.
// That is the point: the command must work everywhere, and "no collector" must
// cost nothing rather than pointing an exporter at 127.0.0.1 and paying one
// stderr line per failed export (the CLI children were part of the 2026-11-08
// journal storm: 238,951 and 213,808 lines from single incarnations).
//
// Safe to call even when the collector isn't configured or reachable — config
// load failure, an unresolvable swarm address or an exporter setup error all
// fall back to a no-op shutdown and the command proceeds with tracing off.
func initCLITelemetry() func() {
	cfg, err := config.Load(configPath)
	if err != nil {
		return func() {}
	}

	// Resolution reads local docker state; bound it so a wedged daemon socket
	// cannot delay a one-shot command.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	endpoint, _ := config.ResolveOTLPEndpoint(ctx, cfg.OTLPEndpoint, docker.NodeSelfAddr, nil)
	cancel()

	shutdown, err := telemetry.Init(context.Background(), telemetry.Options{
		Endpoint:       endpoint,
		ServiceName:    serviceName(),
		ServiceVersion: buildinfo.Version,
	})
	if err != nil {
		return func() {}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}
}

// telemetryFailureLogger renders the exporter's rate-limited failure events as
// daemon log lines. The reporter decides WHEN (first failure, one summary per
// interval, one recovery); this decides only how they read.
func telemetryFailureLogger(log zerolog.Logger) func(telemetry.FailureEvent) {
	return func(ev telemetry.FailureEvent) {
		switch ev.Kind {
		case telemetry.FailureRecovered:
			log.Info().
				Int("suppressed", ev.Suppressed).
				Dur("window", ev.Interval).
				Msg("OTel export recovered")
		case telemetry.FailureOngoing:
			log.Warn().
				Err(ev.Err).
				Int("suppressed", ev.Suppressed).
				Dur("window", ev.Interval).
				Msg("OTel export still failing")
		default:
			log.Warn().
				Err(ev.Err).
				Dur("suppress_for", ev.Interval).
				Msg("OTel export failed; repeats suppressed")
		}
	}
}
