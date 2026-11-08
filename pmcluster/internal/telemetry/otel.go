// Package telemetry owns the OpenTelemetry SDK lifecycle for the
// pmcluster daemon. Init configures TracerProvider + MeterProvider +
// LoggerProvider against the OTLP/HTTP receiver published by the
// observability stack (see internal/cluster/embeds/observability-stack.yml).
//
// The endpoint is never hard-coded here: config.ResolveOTLPEndpoint resolves
// this node's swarm address and dial-checks it, and a node without a reachable
// collector gets NO exporter rather than one pointed at a closed port. Export
// failures are rate-limited by failureReporter, because the SDK's default error
// handler writes one journal line per failed export — unbounded, which is how a
// single daemon boot on a 1-vCPU node produced 261,617 lines and starved the
// container runtime's heartbeat on 2026-11-08.
//
// All providers are registered globally so that callers can use the
// standard otel.Tracer / otel.Meter / global.GetLoggerProvider entry
// points without threading providers through every package.
//
// When the endpoint is empty, Init returns a no-op Shutdown — useful
// for one-shot commands (init, cluster up, deploy, backup create)
// that should never block on the collector being reachable.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Options configures the SDK. Endpoint empty disables everything.
type Options struct {
	// Endpoint is the base URL of the OTLP/HTTP receiver, e.g.
	// "http://10.7.224.15:4318" — always this node's swarm address, resolved
	// by config.ResolveOTLPEndpoint. Empty disables self-telemetry.
	Endpoint string

	// Failures receives the rate-limited export-failure events. nil logs them
	// through the standard logger; the daemon passes a zerolog wrapper so the
	// lines land beside every other daemon log.
	Failures func(FailureEvent)

	// FailureInterval overrides how often a failing exporter may speak.
	// Zero means DefaultFailureLogInterval.
	FailureInterval time.Duration

	// ServiceName is reported as resource attribute service.name.
	// Defaults to "pmcluster".
	ServiceName string

	// ServiceVersion is reported as service.version (use buildinfo).
	ServiceVersion string

	// ServiceNamespace groups related services in OpenObserve. Defaults
	// to "control-plane".
	ServiceNamespace string
}

// Shutdown flushes and shuts down every registered provider with the
// caller-supplied context. Safe to call multiple times.
type Shutdown func(context.Context) error

// Init wires the global TracerProvider, MeterProvider, and
// LoggerProvider against the OTLP/HTTP collector at opts.Endpoint and
// returns a Shutdown the caller must defer.
func Init(ctx context.Context, opts Options) (Shutdown, error) {
	if opts.Endpoint == "" {
		return noopShutdown, nil
	}
	host, secure, err := parseEndpoint(opts.Endpoint)
	if err != nil {
		return noopShutdown, fmt.Errorf("telemetry: %w", err)
	}
	if opts.ServiceName == "" {
		opts.ServiceName = "pmcluster"
	}
	if opts.ServiceNamespace == "" {
		opts.ServiceNamespace = "control-plane"
	}

	// One global handler for every provider: the SDK calls it once per failed
	// export, from its own goroutines, for traces, metrics and logs alike.
	// Installing it before any exporter is built means a collector that is down
	// from the first batch costs one line, not one line per export.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(
		newFailureReporter(opts.FailureInterval, time.Now, probeEndpoint(host), opts.Failures).Handle,
	))

	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(opts.ServiceName),
			semconv.ServiceNamespace(opts.ServiceNamespace),
			semconv.ServiceVersion(opts.ServiceVersion),
		),
	)
	if err != nil {
		return noopShutdown, fmt.Errorf("telemetry: build resource: %w", err)
	}

	traceExp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(host),
		insecureTraceOpt(secure),
	)
	if err != nil {
		return noopShutdown, fmt.Errorf("telemetry: trace exporter: %w", err)
	}
	tp := trace.NewTracerProvider(
		trace.WithBatcher(traceExp, trace.WithBatchTimeout(5*time.Second)),
		trace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	metricExp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpoint(host),
		insecureMetricOpt(secure),
	)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return noopShutdown, fmt.Errorf("telemetry: metric exporter: %w", err)
	}
	mp := metric.NewMeterProvider(
		metric.WithReader(metric.NewPeriodicReader(metricExp, metric.WithInterval(10*time.Second))),
		metric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	logExp, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpoint(host),
		insecureLogOpt(secure),
	)
	if err != nil {
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
		return noopShutdown, fmt.Errorf("telemetry: log exporter: %w", err)
	}
	lp := log.NewLoggerProvider(
		log.WithProcessor(log.NewBatchProcessor(logExp)),
		log.WithResource(res),
	)
	otellog.SetLoggerProvider(lp)

	return func(ctx context.Context) error {
		var errs []error
		if err := tp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("tracer: %w", err))
		}
		if err := mp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("meter: %w", err))
		}
		if err := lp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("logger: %w", err))
		}
		return errors.Join(errs...)
	}, nil
}

// parseEndpoint splits "http://host:port" or "https://host:port" into
// the host:port the OTLP/HTTP exporters expect and a TLS flag.
func parseEndpoint(s string) (host string, secure bool, err error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", false, fmt.Errorf("parse endpoint %q: %w", s, err)
	}
	switch u.Scheme {
	case "http":
		secure = false
	case "https":
		secure = true
	default:
		return "", false, fmt.Errorf("endpoint must be http:// or https://, got %q", s)
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("endpoint missing host: %q", s)
	}
	return u.Host, secure, nil
}

func insecureTraceOpt(secure bool) otlptracehttp.Option {
	if secure {
		return otlptracehttp.WithCompression(otlptracehttp.GzipCompression)
	}
	return otlptracehttp.WithInsecure()
}

func insecureMetricOpt(secure bool) otlpmetrichttp.Option {
	if secure {
		return otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression)
	}
	return otlpmetrichttp.WithInsecure()
}

func insecureLogOpt(secure bool) otlploghttp.Option {
	if secure {
		return otlploghttp.WithCompression(otlploghttp.GzipCompression)
	}
	return otlploghttp.WithInsecure()
}

func noopShutdown(context.Context) error { return nil }
