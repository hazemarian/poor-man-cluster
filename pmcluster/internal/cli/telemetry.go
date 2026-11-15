package cli

import (
	"context"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/noderuntime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/telemetry"
)

// initCLITelemetry wires OTel for one-shot CLI commands so the internal spans
// they emit (deploy, rollback, cluster up, backups) reach this node's collector
// and show up in the observability backend alongside the daemon's HTTP spans.
// Returns a shutdown func that MUST be deferred by the caller so batched spans
// flush before the process exits.
//
// The endpoint is resolved like the daemon's (config.ResolveOTLPEndpoint) with one
// deliberate difference: a one-shot command does not go looking for the collector. Finding
// it means asking the swarm which manager answers on the collector port, which costs a
// docker round-trip and a dial per invocation, and the spans worth having are the daemon's,
// which run for hours. A CLI run therefore exports only when an operator sets an explicit
// otlp_endpoint — and off-cluster (a laptop, a CI runner) it exports nothing at all.
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

	// No role, and no leader endpoint: "auto" resolves to disabled for a one-shot command.
	// An explicit endpoint still overrides it, which is the operator's call.
	endpoint, _ := config.ResolveOTLPEndpoint(cfg.OTLPEndpoint, false, "")

	shutdown, err := telemetry.Init(context.Background(), telemetry.Options{
		Endpoint:       endpoint,
		ServiceName:    noderuntime.ServiceName(),
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
