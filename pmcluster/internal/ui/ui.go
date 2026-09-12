package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/controllers"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/pmapi"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/store"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/views"
)

// App ties the store, pmapi client, renderer, session auth and controllers
// together. Its routes can be mounted into a shared gin engine (the edge
// service combines the operator console with the reverse proxy) or served
// standalone via Handler().
type App struct {
	Cfg   Config
	Store *store.Store
	API   *pmapi.Client
	Auth  *middleware.Auth
	ctrl  *controllers.Controller
}

// NewApp opens the local store, boots the initial user, and wires the UI.
func NewApp(cfg Config) (*App, error) {
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	seedCtx := context.Background()
	// Store the provisioned API URL/token ONCE (first boot) so they survive
	// restarts and are visible/editable in Settings. Respects any existing
	// value, including an operator's explicit clear.
	if cfg.PMAPIToken != "" {
		if err := st.SeedSettingOnce(seedCtx, store.KeyToken, cfg.PMAPIToken); err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("seed api token: %w", err)
		}
	}
	if cfg.PMAPIURL != "" {
		if err := st.SeedSettingOnce(seedCtx, store.KeyAPIURL, cfg.PMAPIURL); err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("seed api url: %w", err)
		}
	}
	renderer, err := views.NewRenderer()
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("renderer: %w", err)
	}
	api := pmapi.New(cfg.PMAPIURL, cfg.PMAPIToken, cfg.UpstreamTimeout)

	if err := bootstrapUsers(context.Background(), st, cfg); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("bootstrap users: %w", err)
	}

	auth := middleware.NewAuth(st, cfg.SessionSecret, cfg.CookieName)
	auth.NudgeSetup = func(ctx context.Context) (bool, error) {
		n, err := st.CountUsers(ctx)
		if err != nil {
			return false, err
		}
		if n == 0 {
			return true, nil
		}
		fu, err := st.FirstUser(ctx)
		if err != nil {
			return false, nil // let login flow explain
		}
		return !fu.PasswordSet, nil
	}

	ctrl := &controllers.Controller{
		Store: st, API: api, Views: renderer, Auth: auth,
		Version: cfg.AppVersion, EnvAPI: cfg.PMAPIURL, EnvToken: cfg.PMAPIToken,
	}

	return &App{Cfg: cfg, Store: st, API: api, Auth: auth, ctrl: ctrl}, nil
}

// Mount registers every UI route on the provided gin engine. This is how the
// edge service combines the operator console with its reverse proxy on one
// HTTP listener.
func (a *App) Mount(engine *gin.Engine) {
	auth := controllers.Auth{Controller: a.ctrl}
	engine.GET("/login", auth.LoginPage)
	engine.POST("/login", auth.Login)
	engine.GET("/setup", auth.SetupPage)
	engine.POST("/setup", auth.Setup)
	engine.POST("/logout", auth.Logout)

	g := engine.Group("")
	g.Use(a.ctrl.Auth.Require())

	ov := controllers.Overview{Controller: a.ctrl}
	g.GET("/", ov.App)
	g.GET("/overview", ov.Fragment)

	st := controllers.Stacks{Controller: a.ctrl}
	g.GET("/stacks", st.List)
	g.GET("/stacks/:name", st.Show)
	g.GET("/stacks/:name/revisions/:rev", st.ShowRevision)
	g.POST("/stacks/:name/rollback", st.Rollback)
	g.GET("/stacks/:name/backups", st.ShowBackups)

	bk := controllers.Backups{Controller: a.ctrl}
	g.GET("/backups", bk.List)
	g.POST("/backups", bk.Create)

	dp := controllers.Deploy{Controller: a.ctrl}
	g.GET("/deploy", dp.Page)
	g.POST("/deploy", dp.Submit)

	stt := controllers.Settings{Controller: a.ctrl}
	g.GET("/settings", stt.Page)
	g.POST("/settings", stt.Save)
}

// Handler returns a gin engine with every UI route. The edge service mounts
// these into its combined router instead; this is used standalone by tests.
func (a *App) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	a.Mount(r)
	return r
}

// bootstrapUsers seeds the initial login. If env user/pass are set, that user
// is created once (password pre-set). Otherwise the first visit needs a no-press
// admin who is nudged to choose a password (see /setup).
func bootstrapUsers(ctx context.Context, st *store.Store, cfg Config) error {
	if cfg.EnvUser != "" && cfg.EnvPass != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(cfg.EnvPass), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		_, err = st.CreateEnvUser(ctx, cfg.EnvUser, string(hash))
		return err
	}
	n, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		_, err = st.CreateUser(ctx, "admin", "", false)
	}
	return err
}

// Run serves the UI on a standalone listener until ctx is cancelled, then shuts
// down gracefully. (In the edge service the UI shares the engine with the
// proxy instead of calling this directly.)
func (a *App) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           a.Handler(),
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
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.Store.Close()
		return srv.Shutdown(sctx)
	case err := <-errCh:
		_ = a.Store.Close()
		return err
	}
}
