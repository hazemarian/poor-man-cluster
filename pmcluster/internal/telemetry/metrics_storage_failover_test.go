package telemetry

import (
	"context"
	"testing"
)

// The three storage-failover alerting metrics are part of the operator
// contract (OpenObserve alerts reference the names), so each Record func is
// pinned to the capture sink with its exact name, value and labels.

func TestRecordStorageFailover(t *testing.T) {
	ctx := context.Background()
	r := newRecorder(t)

	RecordStorageFailover(ctx, "demo", "node-a", "node-b")

	got := r.find(MetricStorageFailover)
	if len(got) != 1 {
		t.Fatalf("captured %d samples for %s, want 1: %+v", len(got), MetricStorageFailover, got)
	}
	s := got[0]
	if s.Value != 1 {
		t.Errorf("value = %d, want 1 (counter increment)", s.Value)
	}
	for k, want := range map[string]string{
		"stack":     "demo",
		"from_node": "node-a",
		"to_node":   "node-b",
	} {
		if s.Labels[k] != want {
			t.Errorf("label %s = %q, want %q (labels: %v)", k, s.Labels[k], want, s.Labels)
		}
	}
	// Nothing else leaked into this metric name.
	if len(r.find(MetricStorageNodeDown)) != 0 || len(r.find(MetricStorageFailoverDisabled)) != 0 {
		t.Errorf("storage failover emission leaked into other metric names: %+v", r.all())
	}
}

func TestRecordStorageNodeDown(t *testing.T) {
	ctx := context.Background()
	r := newRecorder(t)

	RecordStorageNodeDown(ctx, "node-a", 1)
	RecordStorageNodeDown(ctx, "node-a", 0)

	got := r.find(MetricStorageNodeDown)
	if len(got) != 2 {
		t.Fatalf("captured %d samples, want 2 (down + recovered): %+v", len(got), got)
	}
	if got[0].Value != 1 || got[0].Labels["node"] != "node-a" {
		t.Errorf("first sample = %+v, want value 1 node=node-a", got[0])
	}
	if got[1].Value != 0 || got[1].Labels["node"] != "node-a" {
		t.Errorf("second sample = %+v, want value 0 node=node-a", got[1])
	}
	// The gauge is labelled by node only.
	if len(got[0].Labels) != 1 {
		t.Errorf("labels = %v, want only {node}", got[0].Labels)
	}
}

func TestRecordStorageFailoverDisabled(t *testing.T) {
	ctx := context.Background()
	r := newRecorder(t)

	RecordStorageFailoverDisabled(ctx, "demo", "node-a")

	got := r.find(MetricStorageFailoverDisabled)
	if len(got) != 1 {
		t.Fatalf("captured %d samples for %s, want 1: %+v", len(got), MetricStorageFailoverDisabled, got)
	}
	s := got[0]
	if s.Value != 1 {
		t.Errorf("value = %d, want 1", s.Value)
	}
	for k, want := range map[string]string{"stack": "demo", "node": "node-a"} {
		if s.Labels[k] != want {
			t.Errorf("label %s = %q, want %q (labels: %v)", k, s.Labels[k], want, s.Labels)
		}
	}
	if len(r.find(MetricStorageFailover)) != 0 {
		t.Errorf("disabled emission leaked into %s: %+v", MetricStorageFailover, r.all())
	}
}
