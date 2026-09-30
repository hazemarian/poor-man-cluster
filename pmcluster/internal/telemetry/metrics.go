package telemetry

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Alerting metrics emitted by the daemon so operators can build alerts in
// OpenObserve (Q5, decision m4031) without any new infra. The names below
// are part of the operator contract — alert queries reference them — so
// they live here as constants instead of being spelled at each call site.
//
// Every emission goes to the global MeterProvider (a no-op until Init
// registers a real one — hence the lazy instrument construction) and, in
// parallel, to an optional process-wide Sink installed by tests. That
// keeps the metrics hermetically testable with no OTLP collector.
const (
	// MetricWebhookRequests counts webhook deliveries by source and status.
	// Status is one of accepted | unauthorized | bad_request | server_error.
	MetricWebhookRequests = "pmcluster.webhook.requests.total"

	// MetricServicesPaused is the number of services observed with swarm
	// UpdateStatus.State == "paused" on the most recent services listing.
	MetricServicesPaused = "pmcluster.services.paused"

	// MetricStaleImages is the number of services running an image older
	// than StaleImageAge on the most recent services listing.
	MetricStaleImages = "pmcluster.services.stale_images"

	// MetricReconcileTotal counts k8s-style reconcile runs (stack Sync) by
	// stack and status (in_sync | applied | error).
	MetricReconcileTotal = "pmcluster.reconcile.total"
)

// StaleImageAge is how old a service's cached image must be before the
// service counts towards MetricStaleImages. Exported so the counting site
// and the metric contract can't drift apart.
const StaleImageAge = 30 * 24 * time.Hour

// Reconcile statuses for MetricReconcileTotal.
const (
	// ReconcileInSync: the stored manifest re-rendered to the already
	// deployed hash — nothing to apply (no drift).
	ReconcileInSync = "in_sync"
	// ReconcileApplied: drift (or a config/secrets() input change) was
	// re-deployed successfully.
	ReconcileApplied = "applied"
	// ReconcileError: the reconcile run failed.
	ReconcileError = "error"
)

// Sample is one metric emission observed by a Sink.
type Sample struct {
	// Name is the full metric name, e.g. MetricWebhookRequests.
	Name string
	// Value is the increment (counters) or the reading (gauges).
	Value int64
	// Labels are the metric's attribute key/values (source, status, ...).
	Labels map[string]string
}

var (
	sinkMu   sync.RWMutex
	sinkFunc func(Sample)
)

// SetSink installs fn to observe every emission made through this package,
// in addition to the OTLP pipeline, and returns a restore function that
// puts the previous sink back. Tests defer the restore; nil uninstalls.
// Safe for concurrent use.
func SetSink(fn func(Sample)) (restore func()) {
	sinkMu.Lock()
	prev := sinkFunc
	sinkFunc = fn
	sinkMu.Unlock()
	return func() { SetSink(prev) }
}

// capture reports one emission to the installed sink. No sink (the
// production case) is a no-op; sink panics belong to the sink itself.
func capture(name string, value int64, labels map[string]string) {
	sinkMu.RLock()
	fn := sinkFunc
	sinkMu.RUnlock()
	if fn != nil {
		fn(Sample{Name: name, Value: value, Labels: labels})
	}
}

// Instruments are lazily built on first emission so importing this package
// (or emitting before telemetry.Init has run) never caches the no-op meter
// — same pattern as internal/stacks and internal/backups.
var (
	instrOnce       sync.Once
	webhookRequests metric.Int64Counter
	servicesPaused  metric.Int64Gauge
	staleImages     metric.Int64Gauge
	reconcileTotal  metric.Int64Counter
)

func instruments() (metric.Int64Counter, metric.Int64Gauge, metric.Int64Gauge, metric.Int64Counter) {
	instrOnce.Do(func() {
		meter := otel.Meter("github.com/hazemarian/poor-man-cluster/pmcluster/internal/telemetry")
		var err error
		webhookRequests, err = meter.Int64Counter(
			MetricWebhookRequests,
			metric.WithDescription("Webhook delivery outcomes by source. Status is one of accepted|unauthorized|bad_request|server_error — never per-failure-mode, so the 401 indistinguishability invariant stays intact."),
		)
		if err != nil {
			webhookRequests, _ = otel.Meter("noop").Int64Counter("noop")
		}
		servicesPaused, err = meter.Int64Gauge(
			MetricServicesPaused,
			metric.WithDescription("Services observed with swarm UpdateStatus.State=paused on the most recent services listing. scope is \"all\" or one stack name."),
		)
		if err != nil {
			servicesPaused, _ = otel.Meter("noop").Int64Gauge("noop")
		}
		staleImages, err = meter.Int64Gauge(
			MetricStaleImages,
			metric.WithDescription("Services running a local image older than StaleImageAge on the most recent services listing. scope is \"all\" or one stack name."),
		)
		if err != nil {
			staleImages, _ = otel.Meter("noop").Int64Gauge("noop")
		}
		reconcileTotal, err = meter.Int64Counter(
			MetricReconcileTotal,
			metric.WithDescription("K8s-style reconcile (stack Sync) runs by stack and status: in_sync|applied|error."),
		)
		if err != nil {
			reconcileTotal, _ = otel.Meter("noop").Int64Counter("noop")
		}
	})
	return webhookRequests, servicesPaused, staleImages, reconcileTotal
}

// RecordWebhookDelivery counts one webhook delivery outcome labelled
// {source, status}. Called from the receiver at every exit path — success
// and failure alike — regardless of whether delivery history is enabled.
func RecordWebhookDelivery(ctx context.Context, source, status string) {
	c, _, _, _ := instruments()
	c.Add(ctx, 1, metric.WithAttributes(
		attribute.String("source", source),
		attribute.String("status", status),
	))
	capture(MetricWebhookRequests, 1, map[string]string{
		"source": source,
		"status": status,
	})
}

// RecordServiceSnapshot records the two services-listing gauges for one
// observation: how many services have a paused swarm update and how many
// run an image older than StaleImageAge. scope is "all" for the unfiltered
// listing or the stack name for a stack-scoped one; the gauges carry the
// latest reading per scope.
func RecordServiceSnapshot(ctx context.Context, scope string, paused, stale int) {
	_, pg, sg, _ := instruments()
	attrs := metric.WithAttributes(attribute.String("scope", scope))
	pg.Record(ctx, int64(paused), attrs)
	capture(MetricServicesPaused, int64(paused), map[string]string{"scope": scope})
	sg.Record(ctx, int64(stale), attrs)
	capture(MetricStaleImages, int64(stale), map[string]string{"scope": scope})
}

// RecordReconcile counts one reconcile (stack Sync) outcome labelled
// {stack, status} with status one of ReconcileInSync | ReconcileApplied |
// ReconcileError. Exactly one sample per Sync run.
func RecordReconcile(ctx context.Context, stack, status string) {
	_, _, _, rc := instruments()
	rc.Add(ctx, 1, metric.WithAttributes(
		attribute.String("stack", stack),
		attribute.String("status", status),
	))
	capture(MetricReconcileTotal, 1, map[string]string{
		"stack":  stack,
		"status": status,
	})
}
