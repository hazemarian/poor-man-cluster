package observability

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

type nodeReaderFixture struct {
	nodes []runtime.Node
	err   error
}

func (f nodeReaderFixture) NodeList(context.Context) ([]runtime.Node, error) {
	return f.nodes, f.err
}

type metricReaderFixture struct {
	result Result
	err    error
}

func (f metricReaderFixture) NodeMetrics(context.Context, MetricQuery) (Result, error) {
	return f.result, f.err
}
func (f metricReaderFixture) ServiceMetrics(context.Context, MetricQuery) (Result, error) {
	return Result{Status: StatusAvailable}, nil
}

func statusFixture(now time.Time) *ReadService {
	nodes := []runtime.Node{
		{ID: "n1", Hostname: "node-1", Role: "manager", Availability: "active", Status: "ready", IsLeader: true},
		{ID: "n2", Hostname: "node-2", Role: "manager", Availability: "active", Status: "ready"},
		{ID: "n3", Hostname: "node-3", Role: "worker", Availability: "active", Status: "ready"},
	}
	return &ReadService{
		Inventory: nodeReaderFixture{nodes: nodes},
		Now:       func() time.Time { return now },
		Freshness: 5 * time.Minute,
	}
}

func statusWindow(now time.Time) Window {
	return Window{Start: now.Add(-time.Hour), End: now}
}

func TestReadServiceStatusThreeNodeFresh(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	svc := statusFixture(now)
	svc.Metrics = metricReaderFixture{result: Result{
		Status: StatusAvailable,
		Rows: []Row{
			{"host": "node-1", "bucket": now.Add(-time.Minute)},
			{"host": "node-2", "bucket": now.Add(-2 * time.Minute)},
			{"host": "node-3", "bucket": now.Add(-3 * time.Minute)},
		},
	}}

	got, err := svc.Status(context.Background(), statusWindow(now))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFresh || got.TelemetryStatus != StatusFresh {
		t.Fatalf("status = %q/%q, want fresh/fresh", got.Status, got.TelemetryStatus)
	}
	if got.ExpectedNodes != 3 || got.ReportingNodes != 3 {
		t.Fatalf("coverage = %d expected, %d reporting; want 3/3", got.ExpectedNodes, got.ReportingNodes)
	}
	for _, node := range got.Nodes {
		if node.TelemetryStatus != StatusFresh || node.LastSeen.IsZero() {
			t.Errorf("node %q = %q at %v, want fresh timestamp", node.Hostname, node.TelemetryStatus, node.LastSeen)
		}
	}
}

func TestReadServiceStatusUsesActualSampleWithinRoundedBucket(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	svc := statusFixture(now)
	rows := make([]Row, 0, 3)
	for _, host := range []string{"node-1", "node-2", "node-3"} {
		rows = append(rows, Row{
			"host":      host,
			"bucket":    now.Add(-15 * time.Minute),
			"last_seen": now.Add(-time.Minute),
		})
	}
	svc.Metrics = metricReaderFixture{result: Result{Status: StatusAvailable, Rows: rows}}

	got, err := svc.Status(context.Background(), statusWindow(now))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFresh || got.ReportingNodes != 3 {
		t.Fatalf("rounded bucket status = %q with %d reporting nodes, want fresh/3", got.Status, got.ReportingNodes)
	}
	for _, node := range got.Nodes {
		if node.TelemetryStatus != StatusFresh || !node.LastSeen.Equal(now.Add(-time.Minute)) {
			t.Errorf("node %q = %q at %v, want fresh at actual sample", node.Hostname, node.TelemetryStatus, node.LastSeen)
		}
	}
}

func TestReadServiceStatusStaleAndMissingAreDistinct(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	svc := statusFixture(now)
	svc.Metrics = metricReaderFixture{result: Result{
		Status: StatusAvailable,
		Rows: []Row{
			{"host": "node-1", "bucket": now.Add(-10 * time.Minute)},
			{"host": "node-2", "bucket": now.Add(-time.Minute)},
		},
	}}

	got, err := svc.Status(context.Background(), statusWindow(now))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusMissing || got.TelemetryStatus != StatusMissing {
		t.Fatalf("partial status = %q/%q, want missing/missing", got.Status, got.TelemetryStatus)
	}
	if got.ReportingNodes != 2 {
		t.Fatalf("reporting nodes = %d, want 2", got.ReportingNodes)
	}
	statuses := map[string]Status{}
	for _, node := range got.Nodes {
		statuses[node.Hostname] = node.TelemetryStatus
	}
	if statuses["node-1"] != StatusStaleNode || statuses["node-2"] != StatusFresh || statuses["node-3"] != StatusMissing {
		t.Fatalf("node telemetry statuses = %#v, want stale/fresh/missing", statuses)
	}
}

func TestReadServiceStatusTruncatedDoesNotLabelNodesStaleOrMissing(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	svc := statusFixture(now)
	// The metric read hit its row cap, so node-3's newest sample may have been
	// dropped and node-1's retained sample looks old. Neither may be reported
	// as stale or missing from this partial data.
	svc.Metrics = metricReaderFixture{result: Result{
		Status:            StatusAvailable,
		Truncated:         true,
		TruncationReasons: []string{"row_limit"},
		Rows: []Row{
			{"host": "node-1", "bucket": now.Add(-10 * time.Minute)},
			{"host": "node-2", "bucket": now.Add(-time.Minute)},
		},
	}}

	got, err := svc.Status(context.Background(), statusWindow(now))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusUnavailable || got.TelemetryStatus != StatusUnavailable {
		t.Fatalf("truncated status = %q/%q, want unavailable/unavailable", got.Status, got.TelemetryStatus)
	}
	if !got.Truncated || !reflect.DeepEqual(got.TruncationReasons, []string{"row_limit"}) {
		t.Fatalf("truncation = %v reasons = %#v, want true with row_limit", got.Truncated, got.TruncationReasons)
	}
	if got.ExpectedNodes != 3 || len(got.Nodes) != 3 {
		t.Fatalf("inventory = %d expected, %d nodes; want 3/3 retained", got.ExpectedNodes, len(got.Nodes))
	}
	statuses := map[string]Status{}
	for _, node := range got.Nodes {
		statuses[node.Hostname] = node.TelemetryStatus
	}
	if statuses["node-1"] != StatusUnavailable || statuses["node-2"] != StatusFresh || statuses["node-3"] != StatusUnavailable {
		t.Fatalf("node telemetry statuses = %#v, want unavailable/fresh/unavailable", statuses)
	}
}

func TestReadServiceStatusEmptyAndUnavailable(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	window := statusWindow(now)

	t.Run("empty", func(t *testing.T) {
		svc := statusFixture(now)
		svc.Metrics = metricReaderFixture{result: Result{Status: StatusAvailable}}
		got, err := svc.Status(context.Background(), window)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusEmpty || got.TelemetryStatus != StatusEmpty {
			t.Fatalf("status = %q/%q, want empty/empty", got.Status, got.TelemetryStatus)
		}
	})

	t.Run("source unavailable", func(t *testing.T) {
		svc := statusFixture(now)
		svc.Metrics = metricReaderFixture{err: errors.New("backend down")}
		got, err := svc.Status(context.Background(), window)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusUnavailable || got.TelemetryStatus != StatusUnavailable {
			t.Fatalf("status = %q/%q, want unavailable/unavailable", got.Status, got.TelemetryStatus)
		}
	})

	t.Run("inventory failure", func(t *testing.T) {
		svc := &ReadService{Inventory: nodeReaderFixture{err: errors.New("docker unavailable")}, Now: func() time.Time { return now }}
		got, err := svc.Status(context.Background(), window)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusInventoryFailed || got.InventoryStatus != StatusInventoryFailed {
			t.Fatalf("status = %q/%q, want inventory_failed/inventory_failed", got.Status, got.InventoryStatus)
		}
	})
}
