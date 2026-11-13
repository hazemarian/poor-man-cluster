package nodestat

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The metric names below are part of the contract between the daemon (which
// emits them), the collector (which filters them into the file exporter) and
// the leader's reader (which knows nothing else). They are constants here so
// the three can never drift apart silently.
const (
	MetricLoad1  = "pmcluster.node.load.1"
	MetricLoad5  = "pmcluster.node.load.5"
	MetricLoad15 = "pmcluster.node.load.15"

	MetricMemTotalMB = "pmcluster.node.memory.total_mb"
	MetricMemUsedMB  = "pmcluster.node.memory.used_mb"
	MetricMemAvailMB = "pmcluster.node.memory.available_mb"

	MetricCores    = "pmcluster.node.cpu.count"
	MetricDockerUp = "pmcluster.node.docker.up"

	MetricProcessRSS = "pmcluster.process.rss_mb"
	MetricProcessCPU = "pmcluster.process.cpu_pct"
)

// MetricPrefix is what the collector's filter matches to keep node-stats
// series out of the Parseable pipeline and inside the file exporter's.
const MetricPrefix = "pmcluster.node."

// Publish emits one sample to the global MeterProvider as OTLP gauges.
//
// It deliberately creates its instruments per call instead of caching them in
// a sync.Once: the daemon may publish before telemetry.Init has installed the
// real provider (Init is skipped entirely when no endpoint resolves), and a
// once-cached instrument would then be a permanent no-op even after the
// provider appears. Once per collection interval is not a hot path.
//
// host.name and node.role ride on every datapoint as well as on the SDK
// resource: the resource is what the collector's resourcedetection processor
// rewrites to the node's own hostname, and the datapoint copy is what the
// reader falls back to when a fixture (or a future collector) carries no
// resource attributes at all.
func Publish(ctx context.Context, s Sample) {
	m := otel.Meter("github.com/hazemarian/poor-man-cluster/pmcluster/internal/nodestat")
	// THREE node attributes, not one, and each earns its place:
	//   host.name          the OpenTelemetry semantic convention — generic tooling
	//                      (dashboards, collectors, resourcedetection) looks for this
	//   pmcluster.node.name ours, unambiguously namespaced, and the one the platform's
	//                      own queries group by; it cannot be shadowed by a resource
	//                      rewrite the way host.name can
	//   node.role          what the node is, so a panel can separate managers from workers
	//                      without joining against the swarm's node list
	//
	// These ride the DATAPOINT, not only the resource: on the leader's relay the resource
	// is the leader's own, and a point that carried no node attribute would be attributed
	// to the wrong machine — the failure mode that makes a multi-node dashboard lie.
	base := metric.WithAttributes(
		attribute.String("host.name", s.Hostname),
		attribute.String("pmcluster.node.name", s.Hostname),
		attribute.String("node.role", s.Role),
	)

	gauge := func(name, desc string, v float64) {
		g, err := m.Float64Gauge(name, metric.WithDescription(desc))
		if err != nil {
			return
		}
		g.Record(ctx, v, base)
	}
	gauge(MetricLoad1, "1-minute load average of this node.", s.Load1)
	gauge(MetricLoad5, "5-minute load average of this node.", s.Load5)
	gauge(MetricLoad15, "15-minute load average of this node.", s.Load15)
	gauge(MetricMemTotalMB, "Total RAM of this node, MiB.", float64(s.MemTotalMB))
	gauge(MetricMemUsedMB, "RAM in use on this node (total - available), MiB.", float64(s.MemUsedMB))
	gauge(MetricMemAvailMB, "RAM available on this node (MemAvailable), MiB; the question 'will it swap'.", float64(s.MemAvailMB))
	gauge(MetricCores, "Logical CPUs on this node; the denominator of the load tint rule.", float64(s.Cores))
	gauge(MetricDockerUp, "1 when this node's Docker engine socket answers, 0 otherwise.", float64(s.DockerUp))

	// Per-process series. Cardinality is bounded at the source: only the two
	// top lists are exported (at most 2xTopN points per node per interval), so
	// the file the leader tails stays small and the reader's top-5 is a
	// re-rank, not a scrape of every PID on the box.
	rss, rssErr := m.Float64Gauge(MetricProcessRSS, metric.WithDescription("Resident memory of a top-rss process on this node, MiB."))
	if rssErr == nil {
		for _, p := range s.TopRSS {
			rss.Record(ctx, p.RSSMB, metric.WithAttributes(
				attribute.String("host.name", s.Hostname),
				attribute.String("pmcluster.node.name", s.Hostname),
				attribute.Int("process.pid", p.PID),
				attribute.String("process.name", p.Name),
			))
		}
	}
	cpu, cpuErr := m.Float64Gauge(MetricProcessCPU, metric.WithDescription("CPU share of a top-cpu process on this node, percent of one core."))
	if cpuErr == nil {
		for _, p := range s.TopCPU {
			cpu.Record(ctx, p.CPUPct, metric.WithAttributes(
				attribute.String("host.name", s.Hostname),
				attribute.String("pmcluster.node.name", s.Hostname),
				attribute.Int("process.pid", p.PID),
				attribute.String("process.name", p.Name),
			))
		}
	}
}
