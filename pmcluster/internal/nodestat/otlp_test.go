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

// TestPublishEmitsNodeGaugesUnderTheCollectorsNames is the contract that makes a follower
// visible at all: the node metrics go out under the hostmetrics vocabulary
// (system.cpu.utilization{state=idle}, system.memory.utilization{state=used},
// system.cpu.load_average.1m), which is the only set the nodes view and the freshness
// query read. A daemon-only name would land in Parseable and be invisible to every view.
//
// Every point also names its node twice, because the leader relays all four nodes'
// samples through ITS resource: without the datapoint copy, four machines become one.
func TestPublishEmitsNodeGaugesUnderTheCollectorsNames(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	Publish(context.Background(), Sample{
		Hostname: "nxt-sw-6-w", Role: "worker",
		Load1: 1.5, MemTotalMB: 4000, MemUsedMB: 1000,
		CPUIdleFrac: 0.75, CPUIdleKnown: true,
	})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	points := map[string]map[string]string{}
	values := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case MetricSystemCPUUtilization, MetricSystemMemUtilization, MetricSystemLoad1m:
			default:
				continue
			}
			for _, dp := range m.Data.(metricdata.Gauge[float64]).DataPoints {
				attrs := map[string]string{}
				for _, kv := range dp.Attributes.ToSlice() {
					attrs[string(kv.Key)] = kv.Value.Emit()
				}
				points[m.Name] = attrs
				values[m.Name] = dp.Value
			}
		}
	}
	for _, name := range []string{MetricSystemCPUUtilization, MetricSystemMemUtilization, MetricSystemLoad1m} {
		if _, ok := points[name]; !ok {
			t.Fatalf("%s was not published; got %v", name, points)
		}
		if got := points[name]["host.name"]; got != "nxt-sw-6-w" {
			t.Errorf("%s host.name = %q, want the node", name, got)
		}
		if got := points[name]["pmcluster.node.name"]; got != "nxt-sw-6-w" {
			t.Errorf("%s pmcluster.node.name = %q, want the node", name, got)
		}
	}
	if got := points[MetricSystemCPUUtilization]["state"]; got != "idle" {
		t.Errorf("cpu utilization state = %q, want idle (the share that is NOT busy)", got)
	}
	if got := values[MetricSystemCPUUtilization]; got != 0.75 {
		t.Errorf("cpu idle = %v, want 0.75", got)
	}
	if got := points[MetricSystemMemUtilization]["state"]; got != "used" {
		t.Errorf("memory utilization state = %q, want used", got)
	}
	if got := values[MetricSystemMemUtilization]; got != 0.25 {
		t.Errorf("memory used = %v, want 0.25 (1000/4000)", got)
	}
	if got := values[MetricSystemLoad1m]; got != 1.5 {
		t.Errorf("load1 = %v, want 1.5", got)
	}
}

// TestPublishSkipsAnUnknownUtilization: a node whose /proc/stat was unreadable (or that has
// only just started) must publish NO cpu point rather than a 0 that reads as saturated.
func TestPublishSkipsAnUnknownUtilization(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	Publish(context.Background(), Sample{Hostname: "nxt-sw-1-m", Role: "manager", MemTotalMB: 0})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == MetricSystemCPUUtilization || m.Name == MetricSystemMemUtilization {
				t.Fatalf("%s published for a sample with no interval and no memory total", m.Name)
			}
		}
	}
}
