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
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/apikeys"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/certs"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/configs"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/secrets"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/services"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/webhooks"
)

// trustedProxyCIDRs are the networks pmcluster trusts to set
// X-Forwarded-For / X-Real-IP headers.  By default this is localhost
// and standard Docker bridge/overlay gateways.  Traefik runs on the
// same Swarm node, so its IP is typically within the Docker networks.
var trustedProxyCIDRs = []string{
	"127.0.0.1/8",
	"::1/128",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
}

// Deps bundles the collaborators a fully-wired server needs. Optional
// fields cause their associated routes to be omitted when nil, so tests
// can wire a partial server.
type Deps struct {
	Lookup        auth.Lookup
	Docker        runtime.Client
	Store         *store.Store
	DeployService stacks.Deployer
	Cipher        *credentials.Cipher
	Backups       backups.Service

	// HostCerts exposes per-host TLS cert management under /api/tls/hosts.
	// Optional; when nil those routes are omitted.
	HostCerts *certs.HTTP

	// SiteCert exposes management of the cluster's own (main-domain) TLS
	// certificate under /api/tls/site. Optional; when nil those routes are
	// omitted.
	SiteCert *certs.HTTP

	// Update exposes POST /api/update, which re-runs `cluster update`
	// (content-aware re-apply of the platform stacks). Optional; when nil
	// the route is omitted.
	Update *UpdateService

	// Webhooks exposes webhook-source management under /api/webhooks.
	// Optional; when nil those routes are omitted.
	Webhooks webhooks.Service

	// WebhookSources supplies the receiver with the decrypted secret
	// material for HMAC verification. Local-only by design; when nil the
	// /webhook receiver is omitted.
	WebhookSources webhooks.SourceReader

	// APIKeys exposes API-token (user) management under /api/api_keys.
	// Optional; when nil those routes are omitted.
	APIKeys apikeys.Service

	// Configs exposes config CRUD via the configs service. Optional; when
	// nil those routes are omitted.
	Configs configs.Service

	// Secrets exposes secret management via the secrets service. Optional;
	// when nil those routes are omitted.
	Secrets secrets.Service

	// Services exposes whitelisted per-service operations (list, task
	// history, logs, restart, exec) under /api/services. Optional; when nil
	// those routes are omitted.
	Services services.Service
}

func New(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(trustedRealIP)
	r.Use(middleware.RequestID)

	r.Use(httpStatusSpan)

	cfg := defaultRateConfig()
	r.Use(rateLimiter(
		newPerIPRateLimiter(rate.Limit(cfg.generalRate), cfg.generalBurst),
		newPerIPRateLimiter(rate.Limit(cfg.webhookRate), cfg.webhookBurst),
	))

	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/health", api.Health)

	if d.WebhookSources != nil && d.DeployService != nil {
		(&webhooks.Receiver{
			Sources: d.WebhookSources,
			Deploy:  d.DeployService,
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
			(&stacks.HTTP{
				Deploy:  d.DeployService,
				Read:    stacks.Local{Store: d.Store},
				Backups: d.Backups,
			}).Mount(r)
		}
		if d.Backups != nil {
			bh := &backups.HTTP{Svc: d.Backups}
			bh.Mount(r)
			bh.MountStackScoped(r)
		}
		if d.HostCerts != nil {
			d.HostCerts.MountHosts(r)
		}
		if d.SiteCert != nil {
			d.SiteCert.MountSite(r)
		}
		if d.Update != nil {
			d.Update.Mount(r)
		}
		if d.Webhooks != nil {
			(&webhooks.HTTP{Svc: d.Webhooks}).Mount(r)
		}
		if d.APIKeys != nil {
			(&apikeys.HTTP{Svc: d.APIKeys}).Mount(r)
		}
		if d.Secrets != nil {
			(&secrets.HTTP{Svc: d.Secrets}).Mount(r)
		}
		if d.Configs != nil {
			(&configs.HTTP{Svc: d.Configs}).Mount(r)
		}
		if d.Services != nil {
			(&services.HTTP{Svc: d.Services}).Mount(r)
		}
	})

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

	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}
	rIP := net.ParseIP(remoteIP)
	if rIP == nil {
		return ""
	}

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

	for _, hdr := range []string{"True-Client-IP", "X-Real-IP", "X-Forwarded-For"} {
		v := r.Header.Get(hdr)
		if v == "" {
			continue
		}

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
