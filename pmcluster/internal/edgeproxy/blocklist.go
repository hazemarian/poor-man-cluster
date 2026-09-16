package edgeproxy

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// banStore is a time-bounded set of banned IPs. It backs both the static
// BannedCIDRs and the auto-ban ledger.
type banStore struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func newBanStore() *banStore {
	return &banStore{until: make(map[string]time.Time)}
}

// banned reports whether ip is currently banned (not yet expired).
func (b *banStore) banned(ip string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	exp, ok := b.until[ip]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(b.until, ip)
		return false
	}
	return true
}

// ban blocks ip for duration.
func (b *banStore) ban(ip string, d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.until[ip] = time.Now().Add(d)
}

// blocklist evaluates the static allow/deny lists plus the live auto-ban store
// for each request.
type blocklist struct {
	allowed []*net.IPNet
	denied  []*net.IPNet
	banned  *banStore
}

func newBlocklist(cfg Config, banned *banStore) *blocklist {
	return &blocklist{
		allowed: parseNets(cfg.AllowedCIDRs),
		denied:  parseNets(cfg.BannedCIDRs),
		banned:  banned,
	}
}

func (b *blocklist) isBlocked(ip string) bool {
	if b.banned.banned(ip) {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return true
	}
	for _, d := range b.denied {
		if d.Contains(parsed) {
			return true
		}
	}

	if len(b.allowed) > 0 {
		for _, a := range b.allowed {
			if a.Contains(parsed) {
				return false
			}
		}
		return true
	}
	return false
}

// sharedBan is the mutable state both blocklist and auto-ban middleware read
// and write. Created once per handler and shared so a ban takes effect at the
// blocklist layer immediately.
type sharedBan struct {
	store *banStore
}

// Blocklist returns middleware that rejects blocked client IPs with 403.
// /health and the UI path always pass (the UI is separately basic-auth'd).
func Blocklist(cfg Config, sb *sharedBan) func(http.Handler) http.Handler {
	bl := newBlocklist(cfg, sb.store)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := realIP(r.Context())
			if ip == "" {
				ip = hostOnly(r.RemoteAddr)
			}
			if bl.isBlocked(ip) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// autoBan tracks per-IP failure reputation and flips repeat offenders into the
// shared ban store. A "failure" is a rate-limit 429 emitted by the inner chain,
// or an upstream 401/403 (auth/HMAC rejection) coming back from the daemon.
// It runs OUTSIDE the rate limiter so it can observe those 429s.
type autoBan struct {
	threshold int
	duration  time.Duration
	fails     map[string]int
	mu        sync.Mutex
	store     *banStore
}

func newAutoBan(cfg Config, store *banStore) *autoBan {
	return &autoBan{
		threshold: cfg.BanThreshold,
		duration:  cfg.BanDuration,
		fails:     make(map[string]int),
		store:     store,
	}
}

// AutoBan returns middleware that inspects the inner response status and
// records failures by real client IP, banning repeat offenders.
func AutoBan(cfg Config, sb *sharedBan) func(http.Handler) http.Handler {
	ab := newAutoBan(cfg, sb.store)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			ip := realIP(r.Context())
			if ip != "" && isFailureStatus(rec.status) {
				ab.recordFailure(ip)
			}
		})
	}
}

func (a *autoBan) recordFailure(ip string) {
	if a.threshold <= 0 {
		return
	}
	a.mu.Lock()
	a.fails[ip]++
	n := a.fails[ip]
	a.mu.Unlock()
	if n >= a.threshold {
		a.store.ban(ip, a.duration)
		a.mu.Lock()
		delete(a.fails, ip)
		a.mu.Unlock()
	}
}

// isFailureStatus reports statuses that indicate a bad actor: too many
// requests (edge 429) or auth/HMAC rejection upstream (401/403).
func isFailureStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return false
}

// statusRecorder captures the status code written by the wrapped handler so a
// wrapping middleware can act on it after proxying completes.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
