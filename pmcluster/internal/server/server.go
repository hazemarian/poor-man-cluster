// Package server wires the chi HTTP router for the pmcluster daemon.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/api"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/deploy"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/webhook"
)

// trustedProxyCIDRs are the networks pmcluster trusts to set
// X-Forwarded-For / X-Real-IP headers.  By default this is localhost
// and standard Docker bridge/overlay gateways.  Traefik runs on the
// same Swarm node, so its IP is typically within the Docker networks.
var trustedProxyCIDRs = []string{
	"127.0.0.1/8",    // localhost
	"::1/128",        // localhost IPv6
	"10.0.0.0/8",     // Docker default bridge + Swarm overlay default
	"172.16.0.0/12",  // Docker bridge (older default)
	"192.168.0.0/16", // Docker bridge (legacy)
}

// Deps bundles the collaborators a fully-wired server needs. Optional
// fields cause their associated routes to be omitted when nil, so tests
// can wire a partial server.
type Deps struct {
	Lookup        auth.Lookup
	Docker        docker.Client
	Store         *store.Store
	DeployService *deploy.Service
	Cipher        *credentials.Cipher
	BackupTrigger api.BackupTrigger

	// HostCerts exposes per-host TLS cert management under /api/tls/hosts.
	// Optional; when nil those routes are omitted.
	HostCerts *HostCertService
}

func New(d Deps) http.Handler {
	r := chi.NewRouter()

	// RealIP first so later middleware (including rate limiter) sees
	// the true client address.  Only trust known proxy CIDRs (Docker
	// networks, localhost) — prevents IP spoofing from untrusted callers.
	r.Use(trustedRealIP)
	r.Use(middleware.RequestID)
	// Map the final HTTP status code onto the otelhttp server span's status
	// (otelhttp leaves it unset otherwise). Runs inside the server span.
	r.Use(httpStatusSpan)

	// Per-IP rate limiting: separate buckets for API and webhook paths.
	cfg := defaultRateConfig()
	r.Use(rateLimiter(
		newPerIPRateLimiter(rate.Limit(cfg.generalRate), cfg.generalBurst),
		newPerIPRateLimiter(rate.Limit(cfg.webhookRate), cfg.webhookBurst),
	))

	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/health", api.Health)

	// /webhook/{source} sits OUTSIDE /api — HMAC IS the auth, no Bearer.
	if d.Store != nil && d.Cipher != nil && d.DeployService != nil {
		(&webhook.Handler{
			Store:   d.Store,
			Cipher:  d.Cipher,
			Service: d.DeployService,
		}).Mount(r)
	}

	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Bearer(d.Lookup))
		r.Get("/me", api.Me)
		if d.Docker != nil {
			r.Get("/cluster/info", api.ClusterInfoHandler(d.Docker))
			r.Get("/nodes", api.NodesHandler(d.Docker))
		}
		if d.Store != nil && d.DeployService != nil {
			(&api.StacksHandler{
				Store:   d.Store,
				Service: d.DeployService,
			}).Mount(r)
		}
		if d.Store != nil {
			bh := &api.BackupsHandler{Store: d.Store, Trigger: d.BackupTrigger}
			bh.Mount(r)
			bh.MountStackScoped(r)
		}
		if d.HostCerts != nil {
			d.HostCerts.Mount(r)
		}
		if d.Store != nil && d.Cipher != nil {
			(&WebhookService{Store: d.Store, Cipher: d.Cipher}).Mount(r)
		}
		if d.Store != nil {
			(&APIKeyService{Store: d.Store}).Mount(r)
		}
	})

	// otelhttp wraps the whole router so every request produces a span
	// + http.server.* metrics keyed by route template (chi populates
	// the route on the context, otelhttp picks it up via the
	// http.route attribute it sets after the chi pattern matches).
	return otelhttp.NewHandler(r, "pmcluster.http")
}

// statusRecorder captures the HTTP status code written by the handler so we
// can map it onto the OTel server span's status.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// httpStatusSpan runs inside the otelhttp server span and maps the final HTTP
// status code onto the span status: >= 400 is Error, everything else Ok (same
// convention as the app fleet). It also tags each request with its origin
// (webhook vs api vs health) so traces are easy to tell apart in OpenObserve.
func httpStatusSpan(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		span := trace.SpanFromContext(r.Context())
		span.SetAttributes(attribute.Int("http.status_code", rec.status))
		span.SetAttributes(attribute.String("request.kind", requestKind(r.URL.Path)))
		if rec.status >= 400 {
			span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", rec.status))
		} else {
			span.SetStatus(codes.Ok, "")
		}
	})
}

// requestKind buckets a request path into webhook / api / health / other so
// traces can be filtered by where the call came from.
func requestKind(path string) string {
	switch {
	case strings.HasPrefix(path, "/webhook/"):
		return "webhook"
	case strings.HasPrefix(path, "/api/"):
		return "api"
	case path == "/health":
		return "health"
	default:
		return "other"
	}
}

// trustedRealIP is like chi's RealIP but only trusts X-Forwarded-For /
// X-Real-IP from known proxy CIDRs (Docker networks, localhost).
// Requests from untrusted sources ignore the proxy headers.
func trustedRealIP(next http.Handler) http.Handler {
	trustedNets := parseTrustedCIDRs()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rip := realIPFromTrusted(r, trustedNets); rip != "" {
			r.RemoteAddr = rip
		}
		next.ServeHTTP(w, r)
	})
}

func parseTrustedCIDRs() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range trustedProxyCIDRs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// realIPFromTrusted extracts the real client IP from proxy headers only
// when the remote peer is a trusted proxy.
func realIPFromTrusted(r *http.Request, trustedNets []*net.IPNet) string {
	// Parse the remote address (strip port).
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}
	rIP := net.ParseIP(remoteIP)
	if rIP == nil {
		return ""
	}

	// Only trust proxy headers from known proxy networks.
	trusted := false
	for _, n := range trustedNets {
		if n.Contains(rIP) {
			trusted = true
			break
		}
	}
	if !trusted {
		return ""
	}

	// Same precedence order as chi's RealIP.
	for _, hdr := range []string{"True-Client-IP", "X-Real-IP", "X-Forwarded-For"} {
		v := r.Header.Get(hdr)
		if v == "" {
			continue
		}
		// X-Forwarded-For: take the first (leftmost) IP.
		if hdr == "X-Forwarded-For" {
			if idx := strings.IndexByte(v, ','); idx >= 0 {
				v = v[:idx]
			}
		}
		v = strings.TrimSpace(v)
		if ip := net.ParseIP(v); ip != nil {
			return v
		}
	}
	return ""
}

// Run starts the HTTP server on addr and blocks until ctx is cancelled,
// then gracefully shuts down with a 10s deadline.
func Run(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			errCh <- nil
		} else {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
