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

// CachedReader memoises bounded full-window reads for readCacheTTL so repeated
// identical reads pay the backend once. Incremental/live log reads (a Since
// bound) and Status are never cached — those are fresh by definition, and a
// cached "live" read would serve stale rows as if they were current.
type CachedReader struct {
	next Reader
	now  func() time.Time

	mu      sync.Mutex
	entries map[readCacheKey]readCacheEntry
	order   []readCacheKey
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

func (c *CachedReader) put(key readCacheKey, result Result) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
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

// cacheResult serves a cached result when one is fresh for key, else calls fn
// and memoises an available answer.
func (c *CachedReader) cacheResult(key readCacheKey, fn func() (Result, error)) (Result, error) {
	if r, ok := c.get(key); ok {
		return r, nil
	}
	r, err := fn()
	if err == nil && r.Status == StatusAvailable {
		c.put(key, r)
	}
	return r, err
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
