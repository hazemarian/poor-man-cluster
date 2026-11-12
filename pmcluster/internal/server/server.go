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

	"github.com/hazemarian/poor-man-cluster/pmcluster/docs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/api"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/apikeys"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/certs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ci"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/configs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/mcpserver"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runners"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/secrets"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/settings"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/usage"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/webhooks"
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

	// Settings exposes the editable cluster settings surface under
	// /api/cluster/settings. Optional; when nil those routes are omitted.
	Settings settings.Service

	// Usage exposes the config/secret → stacks reference graph under
	// /api/usage. Optional; when nil that route is omitted.
	Usage usage.Service

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

	// Observability exposes bounded read-only cluster telemetry under
	// /api/observability. It requires an authenticated viewer, and a
	// stack-scoped token is refused: cluster telemetry is not a tenant surface.
	Observability observability.Reader

	// MCP, when non-nil, mounts the Model Context Protocol endpoint at
	// /api/mcp behind the same Bearer token and stack-scope guard as the REST
	// routes. Tool calls re-enter this router in-process, so the handlers and
	// their middleware are shared rather than duplicated.
	MCP *MCPOptions

	// GitHubApp backs the console-facing app status/Test routes under
	// /api/github/app. Optional; when nil those routes are omitted and the
	// doctor check skips with a reason.
	GitHubApp *githubapp.Local

	// GitHubRunners backs the console's runner list and its kill/restart
	// actions under /api/github/runners. Optional; when nil the routes are
	// omitted (a process without a docker client cannot act on a runner).
	GitHubRunners *runners.Manager

	// CIBoard serves GET /api/ci/runs: what every repo's latest workflow run is
	// doing. Read-only and cached inside the board, so a console page that polls
	// it costs one GitHub call per repo per minute at most. Optional; nil omits
	// the route and the console panel says why.
	CIBoard *ci.Board

	// GitHubBroker serves POST /api/github/token for GitHub Actions. It is
	// mounted OUTSIDE the Bearer subtree (HMAC is its auth) and omitted when
	// nil.
	GitHubBroker *githubapp.Broker
}

// MCPOptions tunes the MCP sub-route.
type MCPOptions struct {
	// AllowOps is the set of write operations ("METHOD /path") an agent may
	// call. Nil means mcpserver.DefaultAllowedOps: update a service-scoped
	// config, nothing else.
	AllowOps map[string]bool
}

// New assembles the chi router from the optional Deps; every nil dependency
// simply omits its routes.
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

	// Public stack-status badges for READMEs — no auth (GitHub camo fetches
	// them). Aggregate health only, read from the DB snapshot the control
	// loop writes (never live Docker), mounted outside the /api Bearer group.
	BadgeMount(r, d.Store)

	if d.WebhookSources != nil && d.DeployService != nil {
		rec, _ := d.WebhookSources.(webhooks.Recorder)
		(&webhooks.Receiver{
			Sources: d.WebhookSources,
			Deploy:  d.DeployService,
			Record:  rec,
			// Q3: transient deploy failures are retried up to 2 extra
			// times, 30s apart; the retry count lands on the delivery row.
			MaxRetries: webhooks.DefaultDeployRetries,
			RetryDelay: webhooks.DefaultDeployRetryDelay,
		}).Mount(r)
	}

	// The Actions token broker is HMAC-authenticated, so it must live OUTSIDE
	// the Bearer-protected /api subtree — a workflow has no bearer token, and
	// giving it one would put cluster-admin credentials in every repo.
	if d.GitHubBroker != nil {
		d.GitHubBroker.Mount(r)
	}

	// The Bearer /api subtree exists only where a token can be resolved. A
	// daemon that serves WITHOUT a store (a swarm WORKER, which has no current
	// control plane and must never seed an empty one) has no tokens to check, so
	// it answers /health locally and nothing else: /api would be an
	// unauthenticated surface otherwise, and auth.Bearer(nil) would panic on the
	// first request.
	if d.Lookup == nil {
		if d.MCP != nil {
			r.Handle("/api/mcp", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "mcp unavailable: this node has no control plane", http.StatusServiceUnavailable)
			}))
		}
	} else {
		r.Route("/api", func(r chi.Router) {
			r.Use(auth.Bearer(d.Lookup))
			// Per-stack token scoping: after the token resolves to a user,
			// refuse everything outside that stack when the user's token is
			// stack-scoped (no-op for unscoped tokens).
			r.Use(stackScopeGuard)
			r.Get("/me", api.Me)
			if d.Docker != nil {
				r.Get("/cluster/info", api.ClusterInfoHandler(d.Docker))
				// Forcing a Swarm election (the CLI's `cluster leader`): a testing
				// affordance for failover, refused unless the node is a manager and
				// the manager set can absorb the temporary demotion.
				r.Post("/cluster/leader/{hostname}", api.ClusterLeaderHandler(d.Docker))
				r.Get("/cluster/apply", api.ApplyRecordHandler(d.Store))
				r.Get("/cluster/versions", api.VersionsHandler(d.Docker))
				r.Get("/doctor", api.DoctorHandler(d.Docker, d.Store, d.Cipher, d.GitHubApp))
				r.Get("/nodes", api.NodesHandler(d.Docker, d.Store))
				if d.Store != nil {
					r.Post("/nodes/{hostname}/storage", api.NodeStorageHandler(d.Docker, d.Store))
					r.Delete("/nodes/{hostname}/storage", api.NodeStorageHandler(d.Docker, d.Store))
					// Adoption is separate from promotion: it copies data and briefly stops
					// the node's stacks, so it is its own explicit request.
					r.Post("/nodes/{hostname}/storage/adopt", api.NodeStorageAdoptHandler(d.Docker, d.Store))
					r.Delete("/nodes/{hostname}/storage/adopt", api.NodeStorageAdoptHandler(d.Docker, d.Store))
				}
			}
			if d.Store != nil {
				// Per-node process visibility for the overview. Store-only on
				// purpose: it must answer when Parseable (and the collector) are
				// down, which is exactly when an operator needs it.
				r.Get("/node-stats", api.NodeStatsHandler(d.Store))
				(&settings.HTTP{Svc: d.Settings}).Mount(r)
				(&usage.HTTP{Svc: d.Usage}).Mount(r)
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
				if d.Store != nil {
					bh.VolumeRoot = func(ctx context.Context) string {
						return d.Store.SettingDefault(ctx, cluster.SettingVolumeRoot(), "")
					}
				}
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
			if d.GitHubApp != nil {
				// Reading the registration is operator work: the card names the
				// installation and its grants, and the Test button mints a token.
				r.With(auth.RequireRole(auth.RoleOperator)).Group(func(or chi.Router) {
					(&githubapp.HTTP{App: d.GitHubApp}).Mount(or)
				})
			}
			if d.GitHubRunners != nil {
				// Killing a runner cancels a job and removes a container, so it is
				// operator work like the app card next to it.
				r.With(auth.RequireRole(auth.RoleOperator)).Group(func(or chi.Router) {
					(&runners.HTTP{Mgr: d.GitHubRunners}).Mount(or)
				})
			}
			if d.CIBoard != nil {
				// A read of CI state, like the runner list next to it: any operator
				// token may look, nobody may change anything through it.
				r.With(auth.RequireRole(auth.RoleOperator)).Group(func(or chi.Router) {
					(&ci.HTTP{Board: d.CIBoard}).Mount(or)
				})
			}
			if d.Configs != nil {
				(&configs.HTTP{Svc: d.Configs}).Mount(r)
			}
			if d.Services != nil {
				(&services.HTTP{Svc: d.Services}).Mount(r)
			}
			if d.Observability != nil {
				r.With(auth.RequireRole(auth.RoleViewer)).Group(func(or chi.Router) {
					(&observability.HTTP{Svc: d.Observability}).Mount(or)
				})
			}
		})
	}

	if d.MCP != nil && d.Lookup != nil {
		// The catalog is derived from the embedded OpenAPI document, which
		// internal/docscheck keeps in step with these handlers. A broken spec
		// therefore fails the build, not the daemon; if construction still
		// fails, the route answers 500 with the reason instead of vanishing.
		h, err := mcpserver.Handler(r, mcpserver.Options{
			Spec:     docs.OpenAPI,
			AllowOps: d.MCP.AllowOps,
			Version:  buildinfo.Version,
		})
		if err != nil {
			h = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "mcp unavailable: "+err.Error(), http.StatusInternalServerError)
			})
		}
		r.Handle("/api/mcp", auth.Bearer(d.Lookup)(stackScopeGuard(h)))
	}

	base := otelhttp.NewHandler(r, "pmcluster.http")

	// The interactive-exec websocket endpoint CANNOT live inside the chi
	// router: httpStatusSpan's statusRecorder and the otelhttp wrapper do not
	// implement http.Hijacker, which gorilla's Upgrade requires. Dispatch it
	// at the top level — same auth chain (Bearer + stack scope guard), but a
	// real http.ResponseWriter.
	if d.Services != nil && d.Lookup != nil {
		ws := execWSHandler(d)
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if strings.HasPrefix(req.URL.Path, execWSPathPrefix) && strings.HasSuffix(req.URL.Path, execWSPathSuffix) {
				ws.ServeHTTP(w, req)
				return
			}
			base.ServeHTTP(w, req)
		})
	}
	return base
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
