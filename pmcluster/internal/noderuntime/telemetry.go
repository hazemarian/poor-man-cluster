package noderuntime

import (
	"os"
	"strings"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/telemetry"
)

// serviceName returns the OTel resource service.name. It prefers the
// OTEL_SERVICE_NAME env var so deployments can override it, falling back to
// "pmcluster" for local runs.
func ServiceName() string {
	if n := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); n != "" {
		return n
	}
	return "pmcluster"
}

// telemetryFailureLogger renders the exporter's rate-limited failure events as
// daemon log lines. The reporter decides WHEN (first failure, one summary per
// interval, one recovery); this decides only how they read.
func TelemetryFailureLogger(log zerolog.Logger) func(telemetry.FailureEvent) {
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
