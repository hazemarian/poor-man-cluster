package cli

import (
	"context"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/telemetry"
)

// initCLITelemetry wires OTel for one-shot CLI commands so the internal spans
// they emit (deploy, rollback, cluster up, backups) reach the local collector
// (default http://127.0.0.1:4318) and show up in OpenObserve alongside the
// daemon's HTTP spans. Returns a shutdown func that MUST be deferred by the
// caller so batched spans flush before the process exits.
//
// Safe to call even when the collector isn't configured or reachable — config
// load failure or exporter setup error falls back to a no-op shutdown and the
// command proceeds with tracing disabled.
func initCLITelemetry() func() {
	cfg, err := config.Load(configPath)
	endpoint := ""
	if err == nil {
		endpoint = cfg.OTLPEndpoint
	}

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
