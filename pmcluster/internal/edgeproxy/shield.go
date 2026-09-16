package edgeproxy

import (
	"context"
	"net/http"
	"time"
)

// shield guards against connection exhaustion and slow clients. It bounds how
// many requests are in flight at once (a semaphore — saturating it returns 503
// immediately rather than queueing, so a flood can't build a backlog) and caps
// the request body forwarded to the daemon.
type shield struct {
	sem chan struct{}
}

func newShield(maxConcurrent int) *shield {
	if maxConcurrent <= 0 {
		return &shield{sem: nil}
	}
	return &shield{sem: make(chan struct{}, maxConcurrent)}
}

// ConnectionShield returns middleware that acquires a slot on the semaphore
// before the inner handler and releases it after. When full it returns 503
// with a short Retry-After instead of blocking.
func ConnectionShield(cfg Config) func(http.Handler) http.Handler {
	sh := newShield(cfg.MaxConcurrent)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if sh.sem == nil {
				next.ServeHTTP(w, r)
				return
			}
			select {
			case sh.sem <- struct{}{}:
				defer func() { <-sh.sem }()
			default:
				w.Header().Set("Retry-After", "1")
				http.Error(w, "server busy", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequestTimeout wraps the handler with a per-request deadline so a slow
// upstream (or a hung daemon call) can't pin a slot forever.
func RequestTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// LimitBody caps the request body passed to the inner handler. Called last in
// the chain, right before the proxy, so oversized bodies are rejected with 413.
func LimitBody(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && max > 0 {

				r.Body = http.MaxBytesReader(w, r.Body, max)
			}
			next.ServeHTTP(w, r)
		})
	}
}
