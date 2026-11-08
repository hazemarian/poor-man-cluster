package observability

import (
	"context"
	"sync"
	"time"
)

// readCacheTTL is how long a bounded full-window read is served from cache
// before the next read reaches the backend again. Short enough that a freshly
// written log line shows up soon, long enough that a burst of identical reads
// (console page fragments, an agent polling through MCP) pays the backend once.
// It mirrors the console's own 10s evidence cache so the two layers never
// disagree in spirit.
const readCacheTTL = 10 * time.Second

// readCacheMax bounds the cache's memory. Eviction drops expired entries first,
// then the oldest.
const readCacheMax = 64

type readCacheKey struct {
	kind       string
	windowFrom int64
	windowTo   int64
	service    string
	severity   string
	search     string
	limit      int
	offset     int
	bucketNs   int64
	errorsOnly bool
	traceID    string
}

type readCacheEntry struct {
	result Result
	at     time.Time
}

// cachedCall is one backend read that concurrent callers for the same key join.
// Without it a burst — the console's page fragments and an agent polling at the
// same time — pays the backend once per caller, because every caller misses the
// empty cache before any of them has filled it. done is closed only after
// result and err are set, so a joiner reads them without a lock.
type cachedCall struct {
	done   chan struct{}
	result Result
	err    error
}

// CachedReader memoises bounded full-window reads for readCacheTTL so repeated
// identical reads pay the backend once. Incremental/live log reads (a Since
// bound) and Status are never cached — those are fresh by definition, and a
// cached "live" read would serve stale rows as if they were current.
type CachedReader struct {
	next Reader
	now  func() time.Time

	mu       sync.Mutex
	entries  map[readCacheKey]readCacheEntry
	order    []readCacheKey
	inflight map[readCacheKey]*cachedCall
}

// NewCachedReader wraps next with a short memoising cache. A nil next behaves
// like an unavailable reader, so a partially-wired daemon still answers.
func NewCachedReader(next Reader) *CachedReader {
	return &CachedReader{next: next, now: time.Now, entries: map[readCacheKey]readCacheEntry{}}
}

func (c *CachedReader) get(key readCacheKey) (Result, bool) {
	if c == nil {
		return Result{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return Result{}, false
	}
	if c.now().Sub(entry.at) > readCacheTTL {
		delete(c.entries, key)
		c.removeFromOrder(key)
		return Result{}, false
	}
	return entry.result, true
}

// putLocked stores result under key. The caller holds c.mu.
func (c *CachedReader) putLocked(key readCacheKey, result Result) {
	if _, exists := c.entries[key]; exists {
		c.entries[key] = readCacheEntry{result: result, at: c.now()}
		return
	}
	if len(c.entries) >= readCacheMax {
		c.evictOne()
	}
	c.entries[key] = readCacheEntry{result: result, at: c.now()}
	c.order = append(c.order, key)
}

func (c *CachedReader) evictOne() {
	now := c.now()
	for i, key := range c.order {
		if entry, ok := c.entries[key]; ok && now.Sub(entry.at) > readCacheTTL {
			delete(c.entries, key)
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
	oldest := c.order[0]
	delete(c.entries, oldest)
	c.order = c.order[1:]
}

func (c *CachedReader) removeFromOrder(key readCacheKey) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// cacheResult serves a fresh cached result for key, joins an in-flight read of
// the same key, or performs the read itself and memoises an available answer.
//
// The in-flight join is what makes "a burst pays the backend once" true: the
// cache alone only helps a caller that starts after the first response landed,
// and a burst by definition starts before that.
func (c *CachedReader) cacheResult(key readCacheKey, fn func() (Result, error)) (res Result, err error) {
	if c == nil {
		return fn()
	}
	if r, ok := c.get(key); ok {
		return r, nil
	}
	call, leader := c.begin(key)
	if !leader {
		<-call.done
		return call.result, call.err
	}
	// Release the joiners whatever fn does: a panic must not leave them blocked
	// forever on a call that will never finish.
	defer func() {
		if p := recover(); p != nil {
			c.finish(key, call, Result{Status: StatusUnavailable}, ErrUnavailable)
			panic(p)
		}
		c.finish(key, call, res, err)
	}()
	return fn()
}

// begin registers an in-flight read for key, or returns the one already
// running. The caller that gets leader=true must call finish.
func (c *CachedReader) begin(key readCacheKey) (*cachedCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight == nil {
		c.inflight = make(map[readCacheKey]*cachedCall, 4)
	}
	if call, ok := c.inflight[key]; ok {
		return call, false
	}
	call := &cachedCall{done: make(chan struct{})}
	c.inflight[key] = call
	return call, true
}

// finish publishes the read's outcome to every joiner and memoises it when the
// source actually answered.
func (c *CachedReader) finish(key readCacheKey, call *cachedCall, result Result, err error) {
	c.mu.Lock()
	if c.inflight[key] == call {
		delete(c.inflight, key)
	}
	if err == nil && result.Status == StatusAvailable {
		c.putLocked(key, result)
	}
	call.result, call.err = result, err
	c.mu.Unlock()
	// Closing the channel publishes result/err: a joiner's receive happens
	// after this close, which is ordered after the writes above.
	close(call.done)
}

func windowKey(w Window) (int64, int64) { return w.Start.UnixNano(), w.End.UnixNano() }

// Status is never cached: it reads live Swarm inventory plus the newest node
// sample, both of which change outside any read window.
func (c *CachedReader) Status(ctx context.Context, w Window) (StatusResult, error) {
	if c == nil || c.next == nil {
		return StatusResult{Status: StatusUnavailable}, ErrUnavailable
	}
	return c.next.Status(ctx, w)
}

func (c *CachedReader) Logs(ctx context.Context, q LogQuery) (Result, error) {
	if c == nil || c.next == nil {
		return Unavailable(Query{Service: q.Service, Window: q.Window, Limit: q.Limit}, time.Now(), "logs source unavailable"), ErrUnavailable
	}
	// Incremental/live reads carry a Since bound and are fresh by definition.
	if q.Since != "" {
		return c.next.Logs(ctx, q)
	}
	from, to := windowKey(q.Window)
	return c.cacheResult(readCacheKey{
		kind: "logs", windowFrom: from, windowTo: to,
		service: q.Service, severity: q.Severity, search: q.Search, limit: q.Limit, offset: q.Offset,
	}, func() (Result, error) { return c.next.Logs(ctx, q) })
}

func (c *CachedReader) NodeMetrics(ctx context.Context, q MetricQuery) (Result, error) {
	if c == nil || c.next == nil {
		return Unavailable(Query{Window: q.Window, Limit: MaxMetricRows}, time.Now(), "metrics source unavailable"), ErrUnavailable
	}
	from, to := windowKey(q.Window)
	return c.cacheResult(readCacheKey{kind: "node_metrics", windowFrom: from, windowTo: to, bucketNs: int64(q.Bucket)}, func() (Result, error) {
		return c.next.NodeMetrics(ctx, q)
	})
}

func (c *CachedReader) ServiceMetrics(ctx context.Context, q MetricQuery) (Result, error) {
	if c == nil || c.next == nil {
		return Unavailable(Query{Window: q.Window, Limit: MaxMetricRows}, time.Now(), "metrics source unavailable"), ErrUnavailable
	}
	from, to := windowKey(q.Window)
	return c.cacheResult(readCacheKey{kind: "service_metrics", windowFrom: from, windowTo: to, bucketNs: int64(q.Bucket)}, func() (Result, error) {
		return c.next.ServiceMetrics(ctx, q)
	})
}

func (c *CachedReader) Traces(ctx context.Context, q TraceQuery) (Result, error) {
	if c == nil || c.next == nil {
		return Unavailable(Query{Service: q.Service, Window: q.Window, Limit: q.Limit}, time.Now(), "traces source unavailable"), ErrUnavailable
	}
	from, to := windowKey(q.Window)
	return c.cacheResult(readCacheKey{
		kind: "traces", windowFrom: from, windowTo: to,
		service: q.Service, limit: q.Limit, offset: q.Offset, errorsOnly: q.ErrorsOnly,
	}, func() (Result, error) { return c.next.Traces(ctx, q) })
}

func (c *CachedReader) Trace(ctx context.Context, w Window, traceID string) (Result, error) {
	if c == nil || c.next == nil {
		return Unavailable(Query{Window: w, Limit: MaxTraceSpans}, time.Now(), "traces source unavailable"), ErrUnavailable
	}
	from, to := windowKey(w)
	return c.cacheResult(readCacheKey{kind: "trace", windowFrom: from, windowTo: to, traceID: traceID}, func() (Result, error) {
		return c.next.Trace(ctx, w, traceID)
	})
}

func (c *CachedReader) LogVolume(ctx context.Context, q MetricQuery) (Result, error) {
	if c == nil || c.next == nil {
		return Unavailable(Query{Window: q.Window, Limit: MaxMetricRows}, time.Now(), "log volume source unavailable"), ErrUnavailable
	}
	from, to := windowKey(q.Window)
	return c.cacheResult(readCacheKey{kind: "log_volume", windowFrom: from, windowTo: to, bucketNs: int64(q.Bucket)}, func() (Result, error) {
		return c.next.LogVolume(ctx, q)
	})
}

func (c *CachedReader) TraceStats(ctx context.Context, q MetricQuery) (Result, error) {
	if c == nil || c.next == nil {
		return Unavailable(Query{Window: q.Window, Limit: MaxMetricRows}, time.Now(), "trace statistics source unavailable"), ErrUnavailable
	}
	from, to := windowKey(q.Window)
	return c.cacheResult(readCacheKey{kind: "trace_stats", windowFrom: from, windowTo: to, bucketNs: int64(q.Bucket)}, func() (Result, error) {
		return c.next.TraceStats(ctx, q)
	})
}

// ServiceNames is not cached: it is a cheap distinct-query used to offer the
// filter dropdown, and caching a name list buys little.
func (c *CachedReader) ServiceNames(ctx context.Context, w Window) ([]string, error) {
	if c == nil || c.next == nil {
		return nil, ErrUnavailable
	}
	return c.next.ServiceNames(ctx, w)
}

func (c *CachedReader) Alerts(ctx context.Context, q AlertQuery) (Result, error) {
	if c == nil || c.next == nil {
		return Unavailable(Query{Limit: MaxAlertPageSize}, time.Now(), "alerts source unavailable"), ErrUnavailable
	}
	return c.cacheResult(readCacheKey{kind: "alerts", limit: q.Limit}, func() (Result, error) {
		return c.next.Alerts(ctx, q)
	})
}

var _ Reader = (*CachedReader)(nil)
