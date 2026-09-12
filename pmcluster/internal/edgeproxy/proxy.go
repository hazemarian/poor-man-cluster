package edgeproxy

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// hopByHopHeaders are removed from both copy directions so the daemon isn't
// confused by a proxied connection/keep-alive stream.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// New builds the edge http.Handler: routes the pmcluster API/webhook reverse
// proxy (wrapped in the full protection stack). Everything is wired from Config
// so cmd/edge is just a thin server bootstrap.
func New(cfg Config) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	// Resolve the real client IP from Traefik's X-Forwarded-For before any
	// protection middleware that keys on it.
	r.Use(RealIP(cfg.TrustedCIDRs))

	// Shared ban store: blocklist reads it, auto-ban writes it. Created once so
	// a ban takes effect at the blocklist layer on the very next request.
	sb := &sharedBan{store: newBanStore()}

	// Outer protection stack (outer -> inner).
	r.Use(Blocklist(cfg, sb))
	r.Use(ConnectionShield(cfg))
	r.Use(AutoBan(cfg, sb))
	r.Use(RateLimiter(cfg))
	r.Use(RequestTimeout(cfg.RequestTimeout))

	// Build the reverse proxy to the pmcluster daemon.
	upstream, err := url.Parse(cfg.Upstream)
	if err != nil {
		// Config was validated in cmd/edge; a parse failure here is a coding
		// error, so panic loudly rather than serving a dead proxy silently.
		panic("edgeproxy: invalid upstream: " + err.Error())
	}
	proxy := reverseProxy(upstream)
	proxy.ErrorLog = log.New(os.Stderr, "edgeproxy: ", log.LstdFlags)

	// Health is proxied (the daemon answers /health) and exempt from rate limit.
	r.Get("/health", http.HandlerFunc(proxy.ServeHTTP))

	// API + webhook reverse proxy. Everything else (unknown/UI paths) is not
	// proxied — the gin engine serves the console routes ahead of this handler,
	// and any remaining path is a 404 rather than an opaque pass-through.
	r.Handle("/api/*", http.HandlerFunc(proxy.ServeHTTP))
	r.Handle("/webhook/*", http.HandlerFunc(proxy.ServeHTTP))

	return r
}

// reverseProxy configures an httputil.ReverseProxy to the daemon. Rewrite
// rewrites scheme/host, strips hop-by-hop headers, and injects the real client
// IP so the daemon (which runs its own trusted-real-IP logic) sees the genuine
// client via headers it already trusts from overlay peers.
func reverseProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host

			for _, h := range hopByHopHeaders {
				pr.Out.Header.Del(h)
			}
			ip := realIP(pr.In.Context())
			if ip != "" {
				pr.Out.Header.Set("X-Real-IP", ip)
				pr.Out.Header.Set("X-Forwarded-For", ip)
				pr.Out.Header.Set("X-Forwarded-Proto", "https")
			}
		},
	}
}

// ServerTimeouts are the slowloris-protection timeouts applied to the http.Server
// in cmd/edge. Exposed here so unit tests can assert they're sane.
func ServerTimeouts() (header, read, write, idle time.Duration) {
	return 10 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second
}

// Validate checks required config before serving; returns an error the caller
// (cmd/edge) can surface at startup.
func Validate(cfg Config) error {
	if cfg.Upstream == "" {
		return fmt.Errorf("upstream is required")
	}
	if _, err := url.Parse(cfg.Upstream); err != nil {
		return fmt.Errorf("invalid upstream: %w", err)
	}
	return nil
}
