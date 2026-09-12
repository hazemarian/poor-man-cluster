package edgeproxy

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// perIPLimiter is a token-bucket limiter keyed by IP. Unlike the daemon's
// in-memory limiter (internal/server/ratelimit.go), stale entries are evicted
// after an idle window so a public edge doesn't grow unboundedly.
type perIPLimiter struct {
	mu       sync.Mutex
	limiters map[string]*ipEntry
	rate     rate.Limit
	burst    int
	idleTTL  time.Duration
}

type ipEntry struct {
	lim  *rate.Limiter
	last time.Time
}

// newPerIPLimiter builds a limiter with the given rate/burst and an idle
// eviction TTL. TTL <= 0 disables eviction.
func newPerIPLimiter(r float64, burst int, idleTTL time.Duration) *perIPLimiter {
	return &perIPLimiter{
		limiters: make(map[string]*ipEntry),
		rate:     rate.Limit(r),
		burst:    burst,
		idleTTL:  idleTTL,
	}
}

// allow reports whether the IP may proceed. It lazily creates a bucket,
// evicts idle buckets on insert, and updates the last-seen timestamp.
func (p *perIPLimiter) allow(ip string) bool {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	e, ok := p.limiters[ip]
	if !ok {
		if p.idleTTL > 0 {
			for k, old := range p.limiters {
				if now.Sub(old.last) > p.idleTTL {
					delete(p.limiters, k)
				}
			}
		}
		e = &ipEntry{lim: rate.NewLimiter(p.rate, p.burst)}
		p.limiters[ip] = e
	}
	e.last = now
	return e.lim.Allow()
}

// RateLimiter returns middleware applying per-real-IP token buckets. /api and
// /webhook get separate buckets (a noisy CI webhook shouldn't starve the API);
// /health is exempt.
func RateLimiter(cfg Config) func(http.Handler) http.Handler {
	ttl := 10 * time.Minute
	api := newPerIPLimiter(cfg.APIRate, cfg.APIBurst, ttl)
	webhook := newPerIPLimiter(cfg.WebhookRate, cfg.WebhookBurst, ttl)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := realIP(r.Context())
			lim := api
			// /health (daemon liveness, probed by Traefik + the stack healthcheck)
			// is exempt so a throttled client can never wedge health reporting.
			if pathHasPrefix(r.URL.Path, "/health") {
				next.ServeHTTP(w, r)
				return
			}
			if pathHasPrefix(r.URL.Path, "/webhook") {
				lim = webhook
			}
			if ip != "" && !lim.allow(ip) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// pathHasPrefix reports whether p starts with prefix (boundary-aware: either
// exact match or followed by "/").
func pathHasPrefix(p, prefix string) bool {
	if prefix == "" {
		return false
	}
	if p == prefix {
		return true
	}
	return len(p) > len(prefix) && p[:len(prefix)] == prefix && p[len(prefix)] == '/'
}
