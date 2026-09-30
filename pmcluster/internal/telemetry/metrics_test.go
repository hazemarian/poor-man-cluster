package telemetry

import (
	"context"
	"sync"
	"testing"
)

// recorder captures emissions through SetSink so the alerting metrics can
// be asserted hermetically — no OTLP collector, no global MeterProvider
// mutation. The sink is process-wide, so the recorder restores it on
// cleanup; none of these tests call t.Parallel.
type recorder struct {
	mu      sync.Mutex
	got     []Sample
	restore func()
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{}
	r.restore = SetSink(func(s Sample) {
		labels := make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			labels[k] = v
		}
		r.mu.Lock()
		r.got = append(r.got, Sample{Name: s.Name, Value: s.Value, Labels: labels})
		r.mu.Unlock()
	})
	t.Cleanup(func() { r.restore() })
	return r
}

// all returns a copy of every captured sample.
func (r *recorder) all() []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Sample, len(r.got))
	copy(out, r.got)
	return out
}

// find returns the captured samples with the given metric name.
func (r *recorder) find(name string) []Sample {
	var out []Sample
	for _, s := range r.all() {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func TestRecordWebhookDelivery(t *testing.T) {
	ctx := context.Background()
	r := newRecorder(t)

	RecordWebhookDelivery(ctx, "github-prod", "accepted")
	RecordWebhookDelivery(ctx, "github-prod", "unauthorized")
	RecordWebhookDelivery(ctx, "gitlab-ci", "accepted")

	got := r.find(MetricWebhookRequests)
	if len(got) != 3 {
		t.Fatalf("captured %d samples, want 3: %+v", len(got), got)
	}
	want := []map[string]string{
		{"source": "github-prod", "status": "accepted"},
		{"source": "github-prod", "status": "unauthorized"},
		{"source": "gitlab-ci", "status": "accepted"},
	}
	for i, w := range want {
		if got[i].Value != 1 {
			t.Errorf("sample %d value = %d, want 1", i, got[i].Value)
		}
		for k, v := range w {
			if got[i].Labels[k] != v {
				t.Errorf("sample %d label %s = %q, want %q", i, k, got[i].Labels[k], v)
			}
		}
	}
}

func TestRecordServiceSnapshot(t *testing.T) {
	ctx := context.Background()
	r := newRecorder(t)

	RecordServiceSnapshot(ctx, "all", 2, 1)

	paused := r.find(MetricServicesPaused)
	if len(paused) != 1 {
		t.Fatalf("paused samples = %d, want 1", len(paused))
	}
	if paused[0].Value != 2 || paused[0].Labels["scope"] != "all" {
		t.Errorf("paused sample = %+v, want value 2 scope=all", paused[0])
	}

	stale := r.find(MetricStaleImages)
	if len(stale) != 1 {
		t.Fatalf("stale samples = %d, want 1", len(stale))
	}
	if stale[0].Value != 1 || stale[0].Labels["scope"] != "all" {
		t.Errorf("stale sample = %+v, want value 1 scope=all", stale[0])
	}

	// A later observation of the same scope supersedes it (gauges carry the
	// latest reading), and a healthy observation is still emitted — "0" is
	// the alertable state.
	RecordServiceSnapshot(ctx, "demo", 0, 0)
	if got := r.find(MetricServicesPaused); len(got) != 2 {
		t.Fatalf("paused samples after 2nd snapshot = %d, want 2", len(got))
	}
	last := r.find(MetricServicesPaused)[1]
	if last.Value != 0 || last.Labels["scope"] != "demo" {
		t.Errorf("second paused sample = %+v, want value 0 scope=demo", last)
	}
}

func TestRecordReconcile(t *testing.T) {
	ctx := context.Background()
	r := newRecorder(t)

	RecordReconcile(ctx, "demo", ReconcileInSync)
	RecordReconcile(ctx, "demo", ReconcileApplied)
	RecordReconcile(ctx, "demo", ReconcileError)

	got := r.find(MetricReconcileTotal)
	if len(got) != 3 {
		t.Fatalf("captured %d samples, want 3", len(got))
	}
	statuses := make([]string, 0, len(got))
	for _, s := range got {
		if s.Labels["stack"] != "demo" {
			t.Errorf("sample labels = %v, want stack=demo", s.Labels)
		}
		statuses = append(statuses, s.Labels["status"])
	}
	want := []string{ReconcileInSync, ReconcileApplied, ReconcileError}
	for i := range want {
		if statuses[i] != want[i] {
			t.Errorf("status[%d] = %q, want %q", i, statuses[i], want[i])
		}
	}
}

func TestSetSinkRestore(t *testing.T) {
	// Sinks nest: the second SetSink call sits on top of the first, and
	// each returned restore unwinds exactly its own installation — so
	// restores must be applied in reverse (LIFO) order.
	var base, top int
	restoreBase := SetSink(func(Sample) { base++ }) // first install: underneath
	restoreTop := SetSink(func(Sample) { top++ })   // second install: on top

	RecordReconcile(context.Background(), "demo", ReconcileInSync)
	if base != 0 || top != 1 {
		t.Fatalf("with both installed: base=%d top=%d, want only the top sink (0/1)", base, top)
	}

	restoreTop() // unwind the top → the base sink is live again
	RecordReconcile(context.Background(), "demo", ReconcileInSync)
	if base != 1 || top != 1 {
		t.Fatalf("after unwinding top: base=%d top=%d, want base=1 top=1", base, top)
	}

	restoreBase() // unwind the base → no sink at all
	RecordReconcile(context.Background(), "demo", ReconcileInSync)
	if base != 1 || top != 1 {
		t.Fatalf("after unwinding base: base=%d top=%d, want no further emissions", base, top)
	}
}

// TestStaleImageAge is the metric contract: a 30-day threshold. Changing
// it silently would move every OpenObserve staleness alert.
func TestStaleImageAge(t *testing.T) {
	const want = 30 * 24 * 60 * 60 * int64(1_000_000_000) // ns, == 30 days
	if int64(StaleImageAge) != want {
		t.Errorf("StaleImageAge = %d ns, want %d ns (30 days)", int64(StaleImageAge), want)
	}
}
