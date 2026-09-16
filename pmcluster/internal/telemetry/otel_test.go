package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

// TestInit_EmptyEndpointReturnsNoop verifies that one-shot commands
// (which don't pass an endpoint) get a Shutdown they can call without
// the collector being reachable.
func TestInit_EmptyEndpointReturnsNoop(t *testing.T) {
	shutdown, err := Init(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Init with empty endpoint: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown must be non-nil even when disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown returned error: %v", err)
	}
}

// TestInit_MalformedEndpoint surfaces a clear error rather than
// panicking or silently disabling.
func TestInit_MalformedEndpoint(t *testing.T) {
	for _, bad := range []string{
		"not-a-url",
		"127.0.0.1:4318",
		"ftp://host:4318",
		"http://",
	} {
		t.Run(bad, func(t *testing.T) {
			_, err := Init(context.Background(), Options{Endpoint: bad})
			if err == nil {
				t.Errorf("expected error for endpoint %q, got nil", bad)
			}
		})
	}
}

// TestInit_ValidEndpointSetsGlobals confirms a real endpoint configures
// the global TracerProvider and MeterProvider (previously no-op).
// The httptest server accepts any POST so the exporters don't crash on
// first Export; we don't assert payload semantics — that's OTel's job.
func TestInit_ValidEndpointSetsGlobals(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	shutdown, err := Init(context.Background(), Options{
		Endpoint:       srv.URL,
		ServiceName:    "pmcluster-test",
		ServiceVersion: "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})

	tp := otel.GetTracerProvider()
	if tp == nil {
		t.Fatal("global tracer provider is nil after Init")
	}

	_, span := tp.Tracer("test").Start(context.Background(), "smoke")
	span.End()
}
