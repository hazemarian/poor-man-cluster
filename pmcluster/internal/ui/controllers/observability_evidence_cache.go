package controllers

import (
	"sync"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

// evidenceCacheKey identifies one full-panel evidence read. The range string —
// not the concrete window — is part of the key because a cached result is
// deliberately served across the small clock drift between two fragments; the
// exact window is recomputed on each render and only the rows are reused.
type evidenceCacheKey struct {
	service  string
	rangeVal string
	severity string
	search   string
	offset   int
}

type evidenceCacheEntry struct {
	result observability.Result
	at     time.Time
}

const (
	// evidenceCacheTTL trades staleness against load: ~10s is short enough that
	// a freshly written log line shows up soon after it lands, and long enough
	// that a burst of fragments for the same filters (page load, tab switch,
	// refresh) pays Parseable once instead of once per fragment.
	evidenceCacheTTL = 10 * time.Second

	// evidenceCacheMax bounds the cache's memory. Eviction drops expired
	// entries first, then the oldest (front of the insertion order).
	evidenceCacheMax = 64
)

// EvidenceCache memoises full-window evidence reads in-process. Incremental
// (since) and live requests never touch it: those are fresh by definition.
type EvidenceCache struct {
	mu      sync.Mutex
	entries map[evidenceCacheKey]evidenceCacheEntry
	order   []evidenceCacheKey
}

// NewEvidenceCache builds an empty cache.
func NewEvidenceCache() *EvidenceCache {
	return &EvidenceCache{entries: map[evidenceCacheKey]evidenceCacheEntry{}}
}

// Get returns a cached result when one is fresh for key. A nil cache behaves
// like an empty one, so controllers built without a cache still work.
func (c *EvidenceCache) Get(key evidenceCacheKey) (observability.Result, bool) {
	if c == nil {
		return observability.Result{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return observability.Result{}, false
	}
	if time.Since(entry.at) > evidenceCacheTTL {
		delete(c.entries, key)
		c.removeFromOrder(key)
		return observability.Result{}, false
	}
	return entry.result, true
}

// Put stores a fresh result for key, evicting expired entries first and then
// the oldest entry to stay within evidenceCacheMax.
func (c *EvidenceCache) Put(key evidenceCacheKey, result observability.Result) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		c.entries[key] = evidenceCacheEntry{result: result, at: time.Now()}
		return
	}
	if len(c.entries) >= evidenceCacheMax {
		c.evictOne()
	}
	c.entries[key] = evidenceCacheEntry{result: result, at: time.Now()}
	c.order = append(c.order, key)
}

// evictOne drops an expired entry if one exists, else the oldest entry. It is
// called only when the cache is at capacity, so there is always something to
// remove.
func (c *EvidenceCache) evictOne() {
	now := time.Now()
	for i, key := range c.order {
		if e, ok := c.entries[key]; ok && now.Sub(e.at) > evidenceCacheTTL {
			delete(c.entries, key)
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
	oldest := c.order[0]
	delete(c.entries, oldest)
	c.order = c.order[1:]
}

func (c *EvidenceCache) removeFromOrder(key evidenceCacheKey) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}
