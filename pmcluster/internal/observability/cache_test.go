package observability

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// countingReader records how many times each source method is called so tests
// can assert that a cache hit never reaches the backend.
type countingReader struct {
	logs     int32
	metrics  int32
	traces   int32
	trace    int32
	alerts   int32
	status   int32
	names    int32
	delay    time.Duration
	failLogs bool
}

func (f *countingReader) Status(context.Context, Window) (StatusResult, error) {
	atomic.AddInt32(&f.status, 1)
	return StatusResult{Status: StatusAvailable}, nil
}
func (f *countingReader) Logs(_ context.Context, q LogQuery) (Result, error) {
	atomic.AddInt32(&f.logs, 1)
	time.Sleep(f.delay)
	if f.failLogs {
		return Result{Status: StatusUnavailable}, ErrUnavailable
	}
	return Result{Status: StatusAvailable, Rows: []Row{{"body": "x"}}, Window: q.Window}, nil
}
func (f *countingReader) NodeMetrics(_ context.Context, q MetricQuery) (Result, error) {
	atomic.AddInt32(&f.metrics, 1)
	time.Sleep(f.delay)
	return Result{Status: StatusAvailable, Rows: []Row{{"host": "n1"}}, Window: q.Window}, nil
}
func (f *countingReader) ServiceMetrics(_ context.Context, q MetricQuery) (Result, error) {
	atomic.AddInt32(&f.metrics, 1)
	time.Sleep(f.delay)
	return Result{Status: StatusAvailable, Rows: []Row{{"service": "s"}}, Window: q.Window}, nil
}
func (f *countingReader) Traces(_ context.Context, q TraceQuery) (Result, error) {
	atomic.AddInt32(&f.traces, 1)
	time.Sleep(f.delay)
	return Result{Status: StatusAvailable, Rows: []Row{{"trace_id": "t"}}, Window: q.Window}, nil
}
func (f *countingReader) Trace(_ context.Context, w Window, _ string) (Result, error) {
	atomic.AddInt32(&f.trace, 1)
	time.Sleep(f.delay)
	return Result{Status: StatusAvailable, Rows: []Row{{"span_id": "s"}}, Window: w}, nil
}
func (f *countingReader) LogVolume(_ context.Context, q MetricQuery) (Result, error) {
	atomic.AddInt32(&f.metrics, 1)
	return Result{Status: StatusAvailable, Rows: []Row{{"n": 1}}, Window: q.Window}, nil
}
func (f *countingReader) TraceStats(_ context.Context, q MetricQuery) (Result, error) {
	atomic.AddInt32(&f.metrics, 1)
	return Result{Status: StatusAvailable, Rows: []Row{{"traces": 1}}, Window: q.Window}, nil
}
func (f *countingReader) ServiceNames(context.Context, Window) ([]string, error) {
	atomic.AddInt32(&f.names, 1)
	return []string{"svc"}, nil
}
func (f *countingReader) Alerts(_ context.Context, q AlertQuery) (Result, error) {
	atomic.AddInt32(&f.alerts, 1)
	time.Sleep(f.delay)
	return Result{Status: StatusAvailable, Rows: []Row{{"name": "a"}}}, nil
}

func testWindow() Window {
	end := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	return Window{Start: end.Add(-time.Hour), End: end}
}

// TestCachedReaderServesThenExpires: an identical full-window read hits the
// cache, and a read past the TTL reaches the backend again.
func TestCachedReaderServesThenExpires(t *testing.T) {
	now := time.Now()
	f := &countingReader{}
	c := NewCachedReader(f)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	if _, err := c.Logs(ctx, LogQuery{Window: testWindow(), Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Logs(ctx, LogQuery{Window: testWindow(), Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&f.logs); got != 1 {
		t.Fatalf("backend Logs calls = %d, want 1 (second read must be a cache hit)", got)
	}

	// Advance past the TTL: the next read must miss.
	now = now.Add(readCacheTTL + time.Second)
	if _, err := c.Logs(ctx, LogQuery{Window: testWindow(), Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&f.logs); got != 2 {
		t.Fatalf("backend Logs calls after expiry = %d, want 2", got)
	}
}

// TestCachedReaderNeverCachesLiveReads: a Since-bound (incremental/live) read
// always reaches the backend.
func TestCachedReaderNeverCachesLiveReads(t *testing.T) {
	f := &countingReader{}
	c := NewCachedReader(f)
	ctx := context.Background()

	q := LogQuery{Window: testWindow(), Limit: 10, Since: "2026-11-08T11:59:00Z"}
	for i := 0; i < 3; i++ {
		if _, err := c.Logs(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&f.logs); got != 3 {
		t.Fatalf("live Logs calls = %d, want 3 (never cached)", got)
	}
}

// TestCachedReaderNeverCachesUnavailable: a failed read is retried, not
// memoised, so a backend outage heals as soon as the backend does.
func TestCachedReaderNeverCachesUnavailable(t *testing.T) {
	f := &countingReader{failLogs: true}
	c := NewCachedReader(f)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_, _ = c.Logs(ctx, LogQuery{Window: testWindow(), Limit: 10})
	}
	if got := atomic.LoadInt32(&f.logs); got != 2 {
		t.Fatalf("unavailable Logs calls = %d, want 2 (never cached)", got)
	}
}

// TestCachedReaderKeyIsPerQuery: a different filter or window misses the cache.
func TestCachedReaderKeyIsPerQuery(t *testing.T) {
	f := &countingReader{}
	c := NewCachedReader(f)
	ctx := context.Background()
	base := testWindow()

	_, _ = c.Logs(ctx, LogQuery{Window: base, Limit: 10})
	for _, different := range []LogQuery{
		{Window: base, Limit: 10, Service: "other"},
		{Window: base, Limit: 10, Severity: "error"},
		{Window: base, Limit: 10, Search: "needle"},
		{Window: base, Limit: 20},
		{Window: Window{Start: base.Start.Add(-time.Hour), End: base.Start}, Limit: 10},
	} {
		if _, err := c.Logs(ctx, different); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&f.logs); got != 6 {
		t.Fatalf("backend Logs calls = %d, want 6 (each distinct query misses)", got)
	}
}

// TestCachedReaderStatusAndNamesAreNotCached: Status reads live inventory and
// ServiceNames is a cheap helper; both always reach the backend.
func TestCachedReaderStatusAndNamesAreNotCached(t *testing.T) {
	f := &countingReader{}
	c := NewCachedReader(f)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := c.Status(ctx, testWindow()); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ServiceNames(ctx, testWindow()); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&f.status); got != 2 {
		t.Fatalf("Status calls = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&f.names); got != 2 {
		t.Fatalf("ServiceNames calls = %d, want 2", got)
	}
}

// TestCachedReaderCachesTracesMetricsAndAlerts: the other bounded reads memoise
// identically to logs.
func TestCachedReaderCachesTracesMetricsAndAlerts(t *testing.T) {
	f := &countingReader{}
	c := NewCachedReader(f)
	ctx := context.Background()

	_, _ = c.Traces(ctx, TraceQuery{Window: testWindow(), Limit: 5})
	_, _ = c.Traces(ctx, TraceQuery{Window: testWindow(), Limit: 5})
	_, _ = c.NodeMetrics(ctx, MetricQuery{Window: testWindow(), Bucket: time.Minute})
	_, _ = c.NodeMetrics(ctx, MetricQuery{Window: testWindow(), Bucket: time.Minute})
	_, _ = c.Alerts(ctx, AlertQuery{Limit: 100})
	_, _ = c.Alerts(ctx, AlertQuery{Limit: 100})

	if got := atomic.LoadInt32(&f.traces); got != 1 {
		t.Fatalf("Traces calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&f.metrics); got != 1 {
		t.Fatalf("NodeMetrics calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&f.alerts); got != 1 {
		t.Fatalf("Alerts calls = %d, want 1", got)
	}
}

// BenchmarkCachedReaderMissHit measures the memoisation win: a backend read
// that costs ~5ms on a miss costs ~nothing on a hit.
func BenchmarkCachedReaderMissHit(b *testing.B) {
	f := &countingReader{delay: 5 * time.Millisecond}
	c := NewCachedReader(f)
	ctx := context.Background()
	w := testWindow()

	b.Run("miss", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			// A fresh key each iteration forces a backend round trip.
			q := LogQuery{Window: Window{Start: w.Start.Add(time.Duration(i)), End: w.End}, Limit: 10}
			_, _ = c.Logs(ctx, q)
		}
	})
	b.Run("hit", func(b *testing.B) {
		_, _ = c.Logs(ctx, LogQuery{Window: w, Limit: 10}) // prime
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = c.Logs(ctx, LogQuery{Window: w, Limit: 10})
		}
	})
}

// BenchmarkCachedReaderAlertsMissHit and the traces/metrics variants below
// measure the same memoisation win on the newly exposed endpoints, so the
// before/after claim rests on more than one path.
func BenchmarkCachedReaderAlertsMissHit(b *testing.B) {
	f := &countingReader{delay: 5 * time.Millisecond}
	c := NewCachedReader(f)
	ctx := context.Background()

	b.Run("miss", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			// A distinct limit forces a backend round trip each iteration.
			_, _ = c.Alerts(ctx, AlertQuery{Limit: 1 + (i % MaxAlertPageSize)})
		}
	})
	b.Run("hit", func(b *testing.B) {
		_, _ = c.Alerts(ctx, AlertQuery{Limit: 100}) // prime
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = c.Alerts(ctx, AlertQuery{Limit: 100})
		}
	})
}

func BenchmarkCachedReaderTracesMissHit(b *testing.B) {
	f := &countingReader{delay: 5 * time.Millisecond}
	c := NewCachedReader(f)
	ctx := context.Background()
	w := testWindow()

	b.Run("miss", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			q := TraceQuery{Window: Window{Start: w.Start.Add(time.Duration(i)), End: w.End}, Limit: 10}
			_, _ = c.Traces(ctx, q)
		}
	})
	b.Run("hit", func(b *testing.B) {
		_, _ = c.Traces(ctx, TraceQuery{Window: w, Limit: 10}) // prime
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = c.Traces(ctx, TraceQuery{Window: w, Limit: 10})
		}
	})
}
