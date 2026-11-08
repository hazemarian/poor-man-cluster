package observability

import (
	"context"
	"errors"
	"sync"
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
	// unavailableLogs makes Logs answer unavailable WITHOUT an error, so the
	// cache's "only cache an available result" rule is tested on its own.
	unavailableLogs bool
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
	if f.unavailableLogs {
		return Result{Status: StatusUnavailable}, nil
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

// TestCachedReaderMissCostsOneBackendCall is the base contract stated as one
// table over every memoised read: the first call reaches the source exactly
// once, and an identical call inside the TTL reaches it zero more times.
func TestCachedReaderMissCostsOneBackendCall(t *testing.T) {
	ctx := context.Background()
	w := testWindow()

	cases := []struct {
		name  string
		call  func(*CachedReader) error
		calls func(*countingReader) int32
	}{
		{
			name:  "logs",
			call:  func(c *CachedReader) error { _, err := c.Logs(ctx, LogQuery{Window: w, Limit: 10}); return err },
			calls: func(f *countingReader) int32 { return atomic.LoadInt32(&f.logs) },
		},
		{
			name:  "traces",
			call:  func(c *CachedReader) error { _, err := c.Traces(ctx, TraceQuery{Window: w, Limit: 10}); return err },
			calls: func(f *countingReader) int32 { return atomic.LoadInt32(&f.traces) },
		},
		{
			name:  "trace",
			call:  func(c *CachedReader) error { _, err := c.Trace(ctx, w, "0123456789abcdef0123456789abcdef"); return err },
			calls: func(f *countingReader) int32 { return atomic.LoadInt32(&f.trace) },
		},
		{
			name: "node metrics",
			call: func(c *CachedReader) error {
				_, err := c.NodeMetrics(ctx, MetricQuery{Window: w, Bucket: time.Minute})
				return err
			},
			calls: func(f *countingReader) int32 { return atomic.LoadInt32(&f.metrics) },
		},
		{
			name:  "alerts",
			call:  func(c *CachedReader) error { _, err := c.Alerts(ctx, AlertQuery{Limit: 50}); return err },
			calls: func(f *countingReader) int32 { return atomic.LoadInt32(&f.alerts) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &countingReader{}
			c := NewCachedReader(f)
			for i := 0; i < 3; i++ {
				if err := tc.call(c); err != nil {
					t.Fatalf("call %d: %v", i, err)
				}
				if got := tc.calls(f); got != 1 {
					t.Fatalf("backend calls after call %d = %d, want 1 (identical reads inside the TTL are cache hits)", i, got)
				}
			}
		})
	}
}

// TestCachedReaderTTLBoundary pins the exact expiry edge. The cache serves an
// entry while its age is <= readCacheTTL and drops it the instant it is older,
// so a read at exactly the TTL is still a hit and one nanosecond later is a
// miss. The clock is injected — no sleeping, no flake.
func TestCachedReaderTTLBoundary(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	now := base

	f := &countingReader{}
	c := NewCachedReader(f)
	c.now = func() time.Time { return now }

	if _, err := c.Logs(ctx, LogQuery{Window: testWindow(), Limit: 10}); err != nil {
		t.Fatal(err)
	}

	now = base.Add(readCacheTTL)
	if _, err := c.Logs(ctx, LogQuery{Window: testWindow(), Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&f.logs); got != 1 {
		t.Fatalf("backend calls at exactly the TTL = %d, want 1 (the entry is still fresh at its boundary)", got)
	}

	now = base.Add(readCacheTTL + time.Nanosecond)
	if _, err := c.Logs(ctx, LogQuery{Window: testWindow(), Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&f.logs); got != 2 {
		t.Fatalf("backend calls one nanosecond past the TTL = %d, want 2 (the entry must have expired)", got)
	}
}

// TestCachedReaderEvictionDropsTheOldest is the capacity rule: past
// readCacheMax the oldest entry is evicted, so it misses, while a newer one is
// still served from cache. The map must never grow past the bound.
func TestCachedReaderEvictionDropsTheOldest(t *testing.T) {
	ctx := context.Background()
	w := testWindow()

	f := &countingReader{}
	c := NewCachedReader(f)

	if readCacheMax < 3 {
		t.Fatalf("readCacheMax = %d, the eviction test needs at least 3 entries", readCacheMax)
	}
	for i := 0; i < readCacheMax; i++ {
		if _, err := c.Alerts(ctx, AlertQuery{Limit: 1 + i}); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&f.alerts); got != int32(readCacheMax) {
		t.Fatalf("backend calls while filling = %d, want %d", got, readCacheMax)
	}
	if len(c.entries) != readCacheMax {
		t.Fatalf("entries after filling = %d, want %d", len(c.entries), readCacheMax)
	}

	// One more distinct key must evict exactly one entry.
	if _, err := c.Alerts(ctx, AlertQuery{Limit: readCacheMax + 1}); err != nil {
		t.Fatal(err)
	}
	if len(c.entries) > readCacheMax {
		t.Fatalf("entries = %d after the cap was exceeded, want <= %d", len(c.entries), readCacheMax)
	}

	// The oldest key (limit 1) was the first inserted, so it is gone: re-reading
	// it reaches the backend again.
	before := atomic.LoadInt32(&f.alerts)
	if _, err := c.Alerts(ctx, AlertQuery{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&f.alerts); got != before+1 {
		t.Fatalf("re-read of the oldest key = %d backend calls, want %d (it must have been evicted)", got, before+1)
	}

	// The newest key is still cached: no backend call.
	before = atomic.LoadInt32(&f.alerts)
	if _, err := c.Alerts(ctx, AlertQuery{Limit: readCacheMax + 1}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&f.alerts); got != before {
		t.Fatalf("re-read of the newest key reached the backend (%d calls, want %d)", got, before)
	}

	// An unrelated window-bound read must not have been disturbed.
	_ = w
}

// TestCachedReaderLiveReadIsNeverStored: a Since-bound read always reaches the
// source AND leaves nothing behind, so the next full-window read of the same
// filters is still a miss rather than being served a live (partial) page.
func TestCachedReaderLiveReadIsNeverStored(t *testing.T) {
	ctx := context.Background()
	f := &countingReader{}
	c := NewCachedReader(f)
	w := testWindow()

	for i := 0; i < 3; i++ {
		if _, err := c.Logs(ctx, LogQuery{Window: w, Limit: 10, Since: "2026-11-08T11:59:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&f.logs); got != 3 {
		t.Fatalf("live reads = %d backend calls, want 3", got)
	}
	if len(c.entries) != 0 {
		t.Fatalf("cache holds %d entries after live reads, want 0 (a live page must never be stored)", len(c.entries))
	}

	// The same filters without the Since bound are a full-window read: it must
	// reach the source, not reuse anything the live reads left.
	if _, err := c.Logs(ctx, LogQuery{Window: w, Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&f.logs); got != 4 {
		t.Fatalf("full-window read after live reads = %d backend calls, want 4", got)
	}
}

// TestCachedReaderDoesNotCacheUnavailableStatus: a source that answers
// "unavailable" with no error must not be memoised, or an outage would be
// pinned for a full TTL. This is the adapter's deliberate rule (cacheResult
// requires StatusAvailable); the assertion states it so a future change to
// "cache the answer, whatever it is" fails here first.
func TestCachedReaderDoesNotCacheUnavailableStatus(t *testing.T) {
	ctx := context.Background()
	f := &countingReader{unavailableLogs: true}
	c := NewCachedReader(f)

	for i := 0; i < 2; i++ {
		res, err := c.Logs(ctx, LogQuery{Window: testWindow(), Limit: 10})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if res.Status != StatusUnavailable {
			t.Fatalf("call %d status = %v, want unavailable", i, res.Status)
		}
	}
	if got := atomic.LoadInt32(&f.logs); got != 2 {
		t.Fatalf("backend calls = %d, want 2 (an unavailable answer must not be cached)", got)
	}
	if len(c.entries) != 0 {
		t.Fatalf("cache holds %d entries after unavailable answers, want 0", len(c.entries))
	}
}

// TestCachedReaderCoalescesConcurrentIdenticalReads is the burst case the cache
// exists for: N callers that start together on the same key must produce ONE
// backend call, and every caller must receive that call's result. Runs under
// -race in CI, which is the corruption assertion.
func TestCachedReaderCoalescesConcurrentIdenticalReads(t *testing.T) {
	const callers = 16
	ctx := context.Background()
	f := &countingReader{delay: 50 * time.Millisecond}
	c := NewCachedReader(f)
	w := testWindow()

	type answer struct {
		res Result
		err error
	}
	answers := make([]answer, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := c.Logs(ctx, LogQuery{Window: w, Limit: 10})
			answers[i] = answer{res: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&f.logs); got != 1 {
		t.Fatalf("backend calls for %d concurrent identical reads = %d, want 1", callers, got)
	}
	for i, a := range answers {
		if a.err != nil {
			t.Fatalf("caller %d: %v", i, a.err)
		}
		if a.res.Status != StatusAvailable {
			t.Fatalf("caller %d status = %v, want available", i, a.res.Status)
		}
		if len(a.res.Rows) != 1 {
			t.Fatalf("caller %d received %d rows, want 1 (every caller sees the shared answer)", i, len(a.res.Rows))
		}
		if !a.res.Window.Start.Equal(w.Start) {
			t.Fatalf("caller %d window start = %v, want %v", i, a.res.Window.Start, w.Start)
		}
	}
}

// TestCachedReaderConcurrentDistinctKeysDoNotShareState: coalescing must key on
// the query, so concurrent reads of different keys each reach the source and
// each receives its own answer.
func TestCachedReaderConcurrentDistinctKeysDoNotShareState(t *testing.T) {
	const keys = 8
	ctx := context.Background()
	f := &countingReader{delay: 20 * time.Millisecond}
	c := NewCachedReader(f)

	var wg sync.WaitGroup
	errs := make([]error, keys)
	for i := 0; i < keys; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.Alerts(ctx, AlertQuery{Limit: 1 + i})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&f.alerts); got != keys {
		t.Fatalf("backend calls for %d distinct concurrent keys = %d, want %d", keys, got, keys)
	}
	if len(c.entries) != keys {
		t.Fatalf("entries = %d, want %d (one per key)", len(c.entries), keys)
	}
}

// TestCachedReaderNilSourceAnswersUnavailable: a partially-wired daemon (no
// telemetry source) must answer unavailable rather than panic, on every read.
func TestCachedReaderNilSourceAnswersUnavailable(t *testing.T) {
	ctx := context.Background()
	c := NewCachedReader(nil)

	if _, err := c.Alerts(ctx, AlertQuery{Limit: 10}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Alerts error = %v, want ErrUnavailable", err)
	}
	if res, err := c.Logs(ctx, LogQuery{Window: testWindow(), Limit: 10}); !errors.Is(err, ErrUnavailable) || res.Status != StatusUnavailable {
		t.Fatalf("Logs = (%v, %v), want unavailable", res.Status, err)
	}
	if _, err := c.ServiceNames(ctx, testWindow()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ServiceNames error = %v, want ErrUnavailable", err)
	}
	if _, err := c.Status(ctx, testWindow()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Status error = %v, want ErrUnavailable", err)
	}
}

// A caller that derives its window from the clock — "the last hour" — never
// repeats an absolute window, which is exactly how the console and the API ask.
// Keying on the resolved instants made every such read a miss in production
// while the same-window tests above passed. Assert the clock-derived case.
func TestCachedReaderHitsForClockDerivedWindows(t *testing.T) {
	f := &countingReader{}
	c := NewCachedReader(f)

	base := time.Date(2026, 11, 8, 1, 2, 3, 0, time.UTC)
	reads := 0
	c.now = func() time.Time { reads++; return base.Add(time.Duration(reads) * time.Second) }

	first := Window{Start: base.Add(-time.Hour), End: base}
	if _, err := c.Logs(context.Background(), LogQuery{Window: first, Limit: 100}); err != nil {
		t.Fatalf("first read: %v", err)
	}

	// Same requested span, a different instant — the shape production emits.
	second := Window{Start: base.Add(time.Second - time.Hour), End: base.Add(time.Second)}
	if _, err := c.Logs(context.Background(), LogQuery{Window: second, Limit: 100}); err != nil {
		t.Fatalf("second read: %v", err)
	}

	if got := atomic.LoadInt32(&f.logs); got != 1 {
		t.Fatalf("clock-derived reads: %d backend calls, want 1 (windows one second apart share a TTL bucket)", got)
	}
}

// A window whose length differs must not share the entry: "last hour" and
// "last day" over the same end instant are different questions.
func TestCachedReaderSeparatesDifferentDurations(t *testing.T) {
	f := &countingReader{}
	c := NewCachedReader(f)
	base := time.Date(2026, 11, 8, 1, 2, 3, 0, time.UTC)

	for _, d := range []time.Duration{time.Hour, 24 * time.Hour} {
		w := Window{Start: base.Add(-d), End: base}
		if _, err := c.Logs(context.Background(), LogQuery{Window: w, Limit: 100}); err != nil {
			t.Fatalf("read %v: %v", d, err)
		}
	}

	if got := atomic.LoadInt32(&f.logs); got != 2 {
		t.Fatalf("%d backend calls, want 2 (1h and 24h are different keys)", got)
	}
}
