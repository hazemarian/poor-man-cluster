// Command edge runs the pmcluster edge service: the smart reverse proxy AND
// the operator console (gin + HTMX) in one binary, on one port.
//
// The proxy stands in front of the pmcluster daemon, adding per-real-IP
// throttling, a connection/DDoS shield and IP blocklisting with auto-ban. The
// console drives the daemon's API with a Bearer token. Both mount on a single
// gin engine so one Traefik route exposes the whole edge service.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/edgeproxy"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui"
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

	// The operator console shares the edge's upstream (the daemon): one
	// service, one upstream, one origin.
	uiCfg := ui.FromEnv()
	uiCfg.PMAPIURL = proxyCfg.Upstream
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

// combineProxyAndUI builds one gin engine that serves the operator console on
// its web routes and reverse-proxies every other request to the daemon. The
// proxy's full protection stack (real-IP, blocklist, connection shield,
// auto-ban, rate limit) wraps all proxied traffic; only the console routes
// bypass it, and they are gated by their own session auth.
func combineProxyAndUI(proxyCfg edgeproxy.Config, app *ui.App) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())

	// Operator console (gin + HTMX) on this origin.
	app.Mount(engine)

	// Every path the console doesn't claim is reversed to pmcluster through the
	// protection stack: /api/*, /webhook/*, /health and anything else.
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
