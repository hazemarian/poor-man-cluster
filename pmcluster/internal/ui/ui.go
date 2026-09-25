package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/controllers"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/views"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/views/i18n"
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
	// Full page loads of fragment routes (browser refresh / deep link) embed
	// the fragment into the app shell so the console keeps its styles.
	// ShellBase adds language, direction, theme and breadcrumb; the fields
	// below are the ones only the app knows about.
	renderer.ShellData = func(c *gin.Context) gin.H {
		shell := views.ShellBase(c)
		shell["User"] = middleware.CurrentUser(c)
		shell["Version"] = cfg.AppVersion
		shell["LoginDisabled"] = cfg.LoginDisabled
		shell["Domain"] = cfg.ClusterDomain
		return shell
	}
	api := pmapi.New(cfg.PMAPIURL, cfg.PMAPIToken, cfg.UpstreamTimeout)

	if err := bootstrapUsers(context.Background(), st, cfg); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("bootstrap users: %w", err)
	}

	auth := middleware.NewAuth(st, cfg.SessionSecret, cfg.CookieName)
	auth.LoginDisabled = cfg.LoginDisabled
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
		Version: cfg.AppVersion, Domain: cfg.ClusterDomain,
		EnvAPI: cfg.PMAPIURL, EnvToken: cfg.PMAPIToken,
	}

	return &App{Cfg: cfg, Store: st, API: api, Auth: auth, ctrl: ctrl}, nil
}

// Mount registers every UI route on the provided gin engine. This is how the
// edge service combines the operator console with its reverse proxy on one
// HTTP listener.
// WebBase is the URL prefix the operator console is mounted under. Traefik
// gates /web/* with the admin-auth middleware (basicAuth against the
// admin_credentials secret) so the console is only reachable through the
// cluster admin gate; /api/* and /webhook/* fall through to the edge proxy and
// keep their own Bearer/webhook auth.
const WebBase = controllers.WebBase

func (a *App) Mount(engine *gin.Engine) {
	auth := controllers.Auth{Controller: a.ctrl}
	// The bare origin redirects to the console so pmcluster.<domain> still
	// lands on the UI (Traefik routes the non-/web root to the edge; the edge
	// bounces it here).
	engine.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, WebBase+"/")
	})
	// Console assets are public: the sign-in screen needs the stylesheet and
	// the font stack before a session exists.
	engine.GET(views.StaticBase+"/*filepath", views.StaticHandler())
	// Language switch. Public, so it also works from the sign-in screen, and
	// the target is constrained to the console to avoid an open redirect.
	engine.GET(WebBase+"/lang/:code", func(c *gin.Context) {
		lang := string(i18n.Parse(c.Param("code")))
		c.SetCookie(views.CookieLang, lang, 31536000, WebBase, "", false, false)
		next := c.Query("next")
		if !strings.HasPrefix(next, WebBase) {
			next = WebBase + "/"
		}
		c.Redirect(http.StatusFound, next)
	})
	engine.GET(WebBase+"/login", auth.LoginPage)
	engine.POST(WebBase+"/login", auth.Login)
	engine.GET(WebBase+"/setup", auth.SetupPage)
	engine.POST(WebBase+"/setup", auth.Setup)
	engine.POST(WebBase+"/logout", auth.Logout)

	// OpenObserve SSO bridge: served publicly on the observ origin by the edge
	// console. Writes the localStorage envelope OO's SPA needs (it cannot be
	// gated by the console session — the browser has no edge session when it
	// lands here; the Traefik sso-auth middleware upstream is the gate).
	engine.GET("/sso-bridge", (controllers.SSOBridge{Controller: a.ctrl}).Page)

	g := engine.Group(WebBase)
	g.Use(a.ctrl.Auth.Require())

	// ---- viewer: read-only console (all GET page/fragment/list routes) ----
	vr := g.Group("")
	vr.Use(a.ctrl.Auth.RequireRole(store.RoleViewer))

	ov := controllers.Overview{Controller: a.ctrl}
	vr.GET("/", ov.App)
	vr.GET("/overview", ov.Fragment)

	st := controllers.Stacks{Controller: a.ctrl}
	vr.GET("/stacks", st.List)
	vr.GET("/stacks/:name", st.Show)
	vr.GET("/stacks/:name/revisions/:rev", st.ShowRevision)
	vr.GET("/stacks/:name/backups", st.ShowBackups)

	svc := controllers.Services{Controller: a.ctrl}
	vr.GET("/services", svc.List)
	vr.GET("/services/:stack/:service/tasks", svc.Tasks)
	vr.GET("/services/:stack/:service/logs", svc.Logs)

	scc := controllers.StackConfigs{Controller: a.ctrl}
	vr.GET("/stacks/:name/config", scc.Page)
	vr.GET("/stacks/:name/configs/new", scc.ConfigNew)
	vr.GET("/stacks/:name/configs/edit/:config_name", scc.ConfigEdit)
	vr.GET("/stacks/:name/secrets/new", scc.SecretNew)
	vr.GET("/stacks/:name/secrets/edit/:secret_name", scc.SecretEdit)

	bk := controllers.Backups{Controller: a.ctrl}
	vr.GET("/backups", bk.List)
	vr.GET("/backups/:id/files", bk.Browse)

	dp := controllers.Deploy{Controller: a.ctrl}
	vr.GET("/deploy", dp.Page)

	stt := controllers.Settings{Controller: a.ctrl}
	vr.GET("/settings", stt.Page)
	// Cluster-scope configs/secrets: the pages are viewable, the form pages
	// render into the modal, and the mutations live in the operator group.
	vr.GET("/settings/configs/new", stt.ConfigNew)
	vr.GET("/settings/configs/edit/:name", stt.ConfigEdit)
	vr.GET("/settings/secrets/new", stt.SecretNew)
	vr.GET("/settings/secrets/edit/:name", stt.SecretEdit)
	vr.GET("/settings/rendered/:name", stt.RenderedGet)

	tlsC := controllers.TLS{Controller: a.ctrl}
	vr.GET("/tls", tlsC.Page)
	vr.GET("/tls/site/new", tlsC.SiteNew)
	vr.GET("/tls/hosts/new", tlsC.HostNew)

	wh := controllers.Webhooks{Controller: a.ctrl}
	vr.GET("/webhooks", wh.Page)
	vr.GET("/webhooks/:source/deliveries", wh.Deliveries)

	usg := controllers.Usage{Controller: a.ctrl}
	vr.GET("/usage", usg.Page)

	// ---- operator: mutations (sync/rollback/remove, service ops, backups,
	// deploy submit, configs/secrets edits, tls, webhooks) ----
	op := g.Group("")
	op.Use(a.ctrl.Auth.RequireRole(store.RoleOperator))

	op.POST("/stacks/:name/sync", st.Sync)
	op.POST("/stacks/:name/rollback", st.Rollback)
	op.POST("/stacks/:name/remove", st.Remove)

	op.POST("/services/:stack/:service/restart", svc.Restart)
	op.POST("/services/:stack/:service/exec", svc.Exec)

	op.POST("/stacks/:name/configs/add", scc.AddConfig)
	op.POST("/stacks/:name/configs/edit", scc.EditConfig)
	op.POST("/stacks/:name/configs/rollback/:config_name/:version_id", scc.RollbackConfig)
	op.POST("/stacks/:name/configs/remove/:config_name", scc.RemoveConfig)
	op.POST("/stacks/:name/secrets/add", scc.AddSecret)
	op.POST("/stacks/:name/secrets/edit", scc.EditSecret)
	op.POST("/stacks/:name/secrets/remove/:secret_name", scc.RemoveSecret)
	op.GET("/stacks/:name/secrets/reveal/:secret_name", scc.RevealSecret)

	op.POST("/backups", bk.Create)
	op.POST("/backups/:id/restore", bk.Restore)

	op.POST("/deploy", dp.Submit)

	op.POST("/settings/configs/add", stt.AddConfig)
	op.POST("/settings/configs/edit", stt.EditConfig)
	op.POST("/settings/configs/rollback/:name/:version_id", stt.RollbackConfig)
	op.POST("/settings/configs/remove/:name", stt.RemoveConfig)
	op.POST("/settings/secrets/add", stt.AddSecret)
	op.POST("/settings/secrets/edit", stt.EditSecret)
	op.POST("/settings/secrets/remove/:name", stt.RemoveSecret)
	op.GET("/settings/secrets/reveal/:name", stt.RevealSecret)

	op.POST("/tls", tlsC.Add)
	op.POST("/tls/site", tlsC.SetSite)
	op.POST("/tls/remove/:host", tlsC.Remove)

	op.POST("/webhooks", wh.Add)
	op.POST("/webhooks/remove/:source", wh.Remove)

	// ---- admin: API keys, settings save/apply, and the users CRUD ----
	ad := g.Group("")
	ad.Use(a.ctrl.Auth.RequireRole(store.RoleAdmin))

	ak := controllers.APIKeys{Controller: a.ctrl}
	ad.GET("/apikeys", ak.Page)
	ad.POST("/apikeys", ak.Add)
	ad.POST("/apikeys/remove/:id", ak.Remove)

	ad.POST("/settings", stt.Save)
	ad.POST("/settings/apply", stt.Apply)
	ad.GET("/settings/cluster", stt.ClusterSettingsPage)
	ad.POST("/settings/cluster", stt.ClusterSettingsSave)

	us := controllers.Users{Controller: a.ctrl}
	ad.GET("/users", us.List)
	ad.GET("/users/new", us.New)
	ad.GET("/users/edit/:id", us.Edit)
	ad.POST("/users/add", us.Create)
	ad.POST("/users/edit", us.EditSave)
	ad.POST("/users/remove/:id", us.Remove)
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
// is created once (password pre-set, admin role). Otherwise the first-run
// /web/setup page creates the first admin account (username + password) when
// there are no users — no placeholder is pre-created. When login is disabled
// (EDGE_LOGIN_DISABLED) no user rows are created at all — Traefik's admin-auth
// is the only gate, and the user CRUD + setup pages are hidden.
func bootstrapUsers(ctx context.Context, st *store.Store, cfg Config) error {
	if cfg.LoginDisabled {
		return nil
	}
	if cfg.EnvUser != "" && cfg.EnvPass != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(cfg.EnvPass), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		_, err = st.CreateEnvUser(ctx, cfg.EnvUser, string(hash))
		return err
	}
	return nil
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
