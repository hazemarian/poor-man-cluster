// Command edge runs the pmcluster edge service: the smart reverse proxy AND
// the operator console (gin + HTMX) in one binary, on one port.
//
// The proxy stands in front of the pmcluster daemon, adding per-real-IP
// throttling, a connection/DDoS shield and IP blocklisting with auto-ban. The
// console drives the daemon's API with a Bearer token. Both mount on a single
// gin engine so one Traefik route exposes the whole edge service.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/edgeproxy"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/logger"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/logging"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/telemetry"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pmcluster-edge:", err)
		os.Exit(1)
	}
}

func run() error {
	proxyCfg := edgeproxy.FromEnv()

	// Self-telemetry, the same way the daemon and the CLI do it: the console is a service like any
	// other, so it must be able to answer "what did the UI do, and how slow was it" from the
	// observability backend rather than from a shell on the leader.
	//
	// It ran for months with NO telemetry at all: the collector's filelog scrape deliberately skips
	// the edge stack (its access-log volume was noise), and nothing else produced its records — so
	// the console was the one service on the platform you could not investigate. Init runs BEFORE the
	// logger is built, because the OTel log writer captures the global provider when it is created.
	telemetryShutdown := initEdgeTelemetry()
	defer telemetryShutdown()

	// The edge shares PMCLUSTER_LOG_LEVEL with the daemon; its own LOG_LEVEL
	// (set in the edge stack) is the fallback for standalone runs. Every line also goes to the OTel
	// log pipeline as structured JSON, so console logs carry level, service and trace context.
	log := logging.ConfigureFromEnvWithSink(proxyCfg.LogLevel, logger.OTelWriter())
	if err := edgeproxy.Validate(proxyCfg); err != nil {
		return err
	}

	uiCfg := ui.FromEnv()
	uiCfg.PMAPIURL = proxyCfg.Upstream

	// The console is NOT stateless any more, in either mode. It owns an approval
	// table — SSO identities that signed in and are waiting for a role — so an
	// in-memory store would forget every pending request on restart, which is
	// precisely when an operator is trying to see who asked for access. This used
	// to force ":memory:" whenever login was disabled, on the reasoning that a
	// Traefik gate stood in front and the console held nothing worth keeping;
	// both halves of that changed (ADR 0013). DATA_DIR is honoured as-is.

	// The console's own failures (an interactive terminal that cannot dial the daemon, for one)
	// used to leave no trace anywhere: the page showed a status line and the server said nothing.
	uiCfg.Logger = log
	applyEdgeSecrets(&uiCfg)
	if err := uiCfg.Validate(); err != nil {
		return fmt.Errorf("ui config: %w", err)
	}

	app, err := ui.NewApp(uiCfg)
	if err != nil {
		return fmt.Errorf("ui: %w", err)
	}

	// Every outbound call — the console's API client and the reverse proxy's upstream — becomes a
	// client span under the inbound one, so a slow page is a TRACE with a slow hop in it instead of
	// an argument about which half is at fault. The proxy builds its own ReverseProxy with no
	// transport, and the console's API client uses the default one, so instrumenting the default
	// covers both rather than threading a client through two constructors.
	http.DefaultTransport = otelhttp.NewTransport(http.DefaultTransport)

	handler := otelhttp.NewHandler(combineProxyAndUI(proxyCfg, app), "pmcluster-edge")
	srv := &http.Server{
		Addr:              proxyCfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info().
			Str("addr", proxyCfg.ListenAddr).
			Str("upstream", proxyCfg.Upstream).
			Msg("pmcluster-edge listening (proxy + console)")
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		_ = app.Store.Close()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		_ = app.Store.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		log.Info().Msg("pmcluster-edge stopped")
		return nil
	}
}

// initEdgeTelemetry wires the console into the platform's OTel pipeline and returns a flush
// function to defer.
//
// The endpoint comes from OTEL_EXPORTER_OTLP_ENDPOINT, which the edge stack sets to the collector's
// host-published port (the collector and the edge both run on the leader, so host.docker.internal is
// the path from inside the container). Unset means no OTLP: a standalone run — a developer's laptop,
// `pmcluster edge-up` — must not point an exporter at an address nobody answers on, which is the
// shape of the 2026-11-08 journal storm.
//
// PMCLUSTER_NODE names the node the console runs on. Inside a container os.Hostname() is the
// container id, so the stack passes the leader's hostname; without it the console's records would be
// the only ones in the platform with no machine attached.
func initEdgeTelemetry() func() {
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		return func() {}
	}
	shutdown, err := telemetry.Init(context.Background(), telemetry.Options{
		Endpoint:       endpoint,
		ServiceName:    "pmcluster-edge",
		ServiceVersion: buildinfo.Version,
		Node:           strings.TrimSpace(os.Getenv("PMCLUSTER_NODE")),
	})
	if err != nil {
		// Telemetry must never be the reason the console does not start.
		fmt.Fprintln(os.Stderr, "pmcluster-edge: telemetry disabled:", err)
		return func() {}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}
}

// edgeSecretDir is where pmcluster mounts the console's Swarm secrets. Docker
// Swarm mounts each secret as a file named after the secret.
const edgeSecretDir = "/run/secrets"

// applyEdgeSecrets overrides the console config from pmcluster-provisioned
// Swarm secrets when they are mounted. It is a no-op on standalone/local runs
// (no secrets dir), which keep the env-driven config.
func applyEdgeSecrets(cfg *ui.Config) {
	if secret, ok := readSecretFile("edge_ui_secret"); ok {
		cfg.SessionSecret = secret
	}

	if pass, ok := readSecretFile("edge_admin_password"); ok {
		cfg.EnvUser = "admin"
		cfg.EnvPass = string(pass)
	}
	if token, ok := readSecretFile("edge_api_token"); ok {
		cfg.PMAPIToken = string(token)
	}
}

// readSecretFile returns the trimmed contents of a mounted Swarm secret, or ok
// == false when the file is absent (not provisioned).
func readSecretFile(name string) ([]byte, bool) {
	data, err := os.ReadFile(filepath.Join(edgeSecretDir, name))
	if err != nil {
		return nil, false
	}
	return bytes.TrimSpace(data), true
}

// combineProxyAndUI builds one gin engine that serves the operator console on
// its web routes and reverse-proxies every other request to the daemon. The
// proxy's full protection stack (real-IP, blocklist, connection shield,
// auto-ban, rate limit) wraps all proxied traffic; only the console routes
// bypass it, and they are gated by their own session auth.
func combineProxyAndUI(proxyCfg edgeproxy.Config, app *ui.App) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())

	engine.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"edge":   "pmcluster-edge",
		})
	})

	app.Mount(engine)

	engine.NoRoute(gin.WrapH(edgeproxy.New(proxyCfg)))
	return engine
}
