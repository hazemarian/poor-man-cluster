package nodestat

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestPublishCarriesTheNodeNameAsAnAttribute pins the contract for multi-node metrics:
// every datapoint names the node it came from, twice — host.name (the OTel semantic
// convention, for generic tooling) and pmcluster.node.name (ours, the one the platform
// groups by). The leader relays every node's sample through ITS resource, so a point
// without these attributes is attributed to the wrong machine.
func TestPublishCarriesTheNodeNameAsAnAttribute(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	Publish(context.Background(), Sample{
		Hostname: "nxt-sw-6-w", Role: "worker", Cores: 2,
		Load1: 1.5, MemTotalMB: 3852, MemUsedMB: 1200, MemAvailMB: 2652, DockerUp: 1,
		TopRSS: []Proc{{PID: 42, Name: "dockerd", RSSMB: 120}},
	})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != MetricLoad1 {
				continue
			}
			for _, dp := range m.Data.(metricdata.Gauge[float64]).DataPoints {
				found = true
				var host, node string
				for _, kv := range dp.Attributes.ToSlice() {
					switch string(kv.Key) {
					case "host.name":
						host = kv.Value.AsString()
					case "pmcluster.node.name":
						node = kv.Value.AsString()
					}
				}
				if host != "nxt-sw-6-w" || node != "nxt-sw-6-w" {
					t.Fatalf("attributes = host.name=%q pmcluster.node.name=%q, want both to name the node", host, node)
				}
			}
		}
	}
	if !found {
		t.Fatal("no load gauge was published")
	}
}
