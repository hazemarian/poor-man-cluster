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
			return false, nil
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

	// Per-stack configs + secrets: service-scope rows belonging to that stack.
	scc := controllers.StackConfigs{Controller: a.ctrl}
	g.GET("/stacks/:name/config", scc.Page)
	g.POST("/stacks/:name/configs/add", scc.AddConfig)
	g.POST("/stacks/:name/configs/edit", scc.EditConfig)
	g.POST("/stacks/:name/configs/rollback/:config_name/:version_id", scc.RollbackConfig)
	g.POST("/stacks/:name/configs/remove/:config_name", scc.RemoveConfig)
	g.POST("/stacks/:name/secrets/add", scc.AddSecret)
	g.POST("/stacks/:name/secrets/edit", scc.EditSecret)
	g.POST("/stacks/:name/secrets/remove/:secret_name", scc.RemoveSecret)
	g.GET("/stacks/:name/secrets/reveal/:secret_name", scc.RevealSecret)

	bk := controllers.Backups{Controller: a.ctrl}
	g.GET("/backups", bk.List)
	g.POST("/backups", bk.Create)

	dp := controllers.Deploy{Controller: a.ctrl}
	g.GET("/deploy", dp.Page)
	g.POST("/deploy", dp.Submit)

	stt := controllers.Settings{Controller: a.ctrl}
	g.GET("/settings", stt.Page)
	g.POST("/settings", stt.Save)
	// Cluster-scope configs + secrets live in Settings; "Apply to swarm"
	// re-runs the cluster update so edits reach the swarm side.
	g.POST("/settings/configs/add", stt.AddConfig)
	g.POST("/settings/configs/edit", stt.EditConfig)
	g.POST("/settings/configs/rollback/:name/:version_id", stt.RollbackConfig)
	g.POST("/settings/configs/remove/:name", stt.RemoveConfig)
	g.POST("/settings/secrets/add", stt.AddSecret)
	g.POST("/settings/secrets/edit", stt.EditSecret)
	g.POST("/settings/secrets/remove/:name", stt.RemoveSecret)
	g.GET("/settings/secrets/reveal/:name", stt.RevealSecret)
	g.POST("/settings/apply", stt.Apply)

	tlsC := controllers.TLS{Controller: a.ctrl}
	g.GET("/tls", tlsC.Page)
	g.POST("/tls", tlsC.Add)
	g.POST("/tls/site", tlsC.SetSite)
	g.POST("/tls/remove/:host", tlsC.Remove)

	wh := controllers.Webhooks{Controller: a.ctrl}
	g.GET("/webhooks", wh.Page)
	g.POST("/webhooks", wh.Add)
	g.POST("/webhooks/remove/:source", wh.Remove)

	ak := controllers.APIKeys{Controller: a.ctrl}
	g.GET("/apikeys", ak.Page)
	g.POST("/apikeys", ak.Add)
	g.POST("/apikeys/remove/:id", ak.Remove)
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
