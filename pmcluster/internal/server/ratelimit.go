package server

import (
	"net/http"
	"sync"

	"golang.org/x/time/rate"
)

// Default rate limits.  Generous enough for CI-driven deploys; operators
// can tune via rateLimiterConfig before calling New.
const (
	// GeneralAPIRate is per-IP requests per second for /api/* endpoints.
	GeneralAPIRate = 100

	// GeneralAPIBurst is the bucket size for /api/* endpoints.
	GeneralAPIBurst = 200

	// WebhookRate is per-IP requests per second for /webhook/* endpoints.
	WebhookRate = 20

	// WebhookBurst is the bucket size for /webhook/* endpoints.
	WebhookBurst = 30
)

// rateLimiterConfig holds the tunable limits.
type rateLimiterConfig struct {
	generalRate  float64
	generalBurst int
	webhookRate  float64
	webhookBurst int
}

// defaultRateConfig returns the built-in defaults.
func defaultRateConfig() rateLimiterConfig {
	return rateLimiterConfig{
		generalRate:  GeneralAPIRate,
		generalBurst: GeneralAPIBurst,
		webhookRate:  WebhookRate,
		webhookBurst: WebhookBurst,
	}
}

// perIPRateLimiter is a simple in-memory IP → rate.Limiter map.  Stale
// entries are not evicted (pmcluster runs on a manager node with limited
// concurrent clients, so unbounded growth is not a concern in practice).
type perIPRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	rate     rate.Limit
	burst    int
}

func newPerIPRateLimiter(r rate.Limit, b int) *perIPRateLimiter {
	return &perIPRateLimiter{
		limiters: make(map[string]*rate.Limiter),
		rate:     r,
		burst:    b,
	}
}

func (p *perIPRateLimiter) allow(ip string) bool {
	p.mu.Lock()
	lim, ok := p.limiters[ip]
	if !ok {
		lim = rate.NewLimiter(p.rate, p.burst)
		p.limiters[ip] = lim
	}
	p.mu.Unlock()
	return lim.Allow()
}

// rateLimiter returns a middleware that applies per-IP rate limits.
// apiLimiter and webhookLimiter are separate buckets so a noisy CI
// webhook doesn't throttle the admin API.
func rateLimiter(apiLimiter, webhookLimiter *perIPRateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := r.RemoteAddr

			lim := apiLimiter
			if len(r.URL.Path) >= 8 && r.URL.Path[:8] == "/webhook" {
				lim = webhookLimiter
			}
			if !lim.allow(ip) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
