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
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/edgeproxy"
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
	log := newLogger(proxyCfg.LogLevel)
	if err := edgeproxy.Validate(proxyCfg); err != nil {
		return err
	}

	uiCfg := ui.FromEnv()
	uiCfg.PMAPIURL = proxyCfg.Upstream

	applyEdgeSecrets(&uiCfg)
	if err := uiCfg.Validate(); err != nil {
		return fmt.Errorf("ui config: %w", err)
	}
	if string(uiCfg.SessionSecret) == "pmcluster-ui-insecure-default-change-me" {
		log.Warn().Msg("PMCLUSTER_UI_SECRET not set — using the insecure default. Set it in production.")
	}

	app, err := ui.NewApp(uiCfg)
	if err != nil {
		return fmt.Errorf("ui: %w", err)
	}

	handler := combineProxyAndUI(proxyCfg, app)
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

// newLogger returns a colorized zerolog console logger at the given level.
func newLogger(level string) zerolog.Logger {
	lvl := zerolog.InfoLevel
	switch level {
	case "debug":
		lvl = zerolog.DebugLevel
	case "warn":
		lvl = zerolog.WarnLevel
	case "error":
		lvl = zerolog.ErrorLevel
	}
	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}).
		Level(lvl).
		With().Timestamp().Logger()
}
