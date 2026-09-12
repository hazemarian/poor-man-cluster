package controllers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/store"
)

// Auth handles sign-in, the first-run password setup, and sign-out.
type Auth struct{ *Controller }

// needSetup reports whether a bootstrap admin still awaits a password (first run).
func (c Auth) needSetup(ctx context.Context) (bool, error) {
	n, err := c.Store.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return true, nil
	}
	fu, err := c.Store.FirstUser(ctx)
	if err != nil {
		return false, err
	}
	return !fu.PasswordSet, nil
}

type loginData struct {
	Version string
	Error   string
}

// LoginPage renders the sign-in form.
func (c Auth) LoginPage(g *gin.Context) {
	c.Views.Page(g, "login", loginData{Version: c.Version})
}

// Login verifies credentials and issues a session cookie.
func (c Auth) Login(g *gin.Context) {
	username := g.PostForm("username")
	password := g.PostForm("password")

	u, err := c.Store.GetByUsername(g.Request.Context(), username)
	if err != nil || !u.PasswordSet || !checkPassword(u, password) {
		c.Views.Page(g, "login", loginData{Version: c.Version, Error: "Invalid username or password."})
		return
	}
	c.Auth.SetCookie(g, u.Username, 7*24*time.Hour)
	redirect(g, "/")
}

// SetupPage renders the first-run set-password form. It only makes sense while
// the bootstrap admin has no password; otherwise bounce to login.
func (c Auth) SetupPage(g *gin.Context) {
	need, err := c.needSetup(g.Request.Context())
	if err != nil || !need {
		redirect(g, "/login")
		return
	}
	uname := ""
	if fu, e := c.Store.FirstUser(g.Request.Context()); e == nil {
		uname = fu.Username
	}
	c.Views.Page(g, "setup", gin.H{"Username": uname})
}

// Setup sets the bootstrap admin's password on first run.
func (c Auth) Setup(g *gin.Context) {
	need, err := c.needSetup(g.Request.Context())
	if err != nil || !need {
		redirect(g, "/login")
		return
	}
	pass := g.PostForm("password")
	confirm := g.PostForm("confirm")
	if len(pass) < 8 || pass != confirm {
		c.Views.Page(g, "setup", gin.H{"Error": "Password must be at least 8 characters and match the confirmation."})
		return
	}
	fu, err := c.Store.FirstUser(g.Request.Context())
	if err != nil {
		http.Error(g.Writer, "no user to set a password for", http.StatusInternalServerError)
		return
	}
	if err := c.Store.SetPassword(g.Request.Context(), fu.Username, hashPassword(pass)); err != nil {
		http.Error(g.Writer, "could not save password", http.StatusInternalServerError)
		return
	}
	c.Auth.SetCookie(g, fu.Username, 7*24*time.Hour)
	c.Views.Page(g, "setup", gin.H{"Username": fu.Username, "Done": true})
}

// Logout clears the session cookie and returns to the sign-in screen.
func (c Auth) Logout(g *gin.Context) {
	c.Auth.ClearCookie(g)
	redirect(g, "/login")
}

func hashPassword(p string) string {
	b, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	if err != nil {
		return ""
	}
	return string(b)
}

func checkPassword(u *store.User, p string) bool {
	if u.PasswordHash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(p)) == nil
}

// redirect sends a browser (or an HTMX partial) to a new location. For HTMX we
// use HX-Redirect so the whole tab navigates rather than swapping a fragment.
func redirect(g *gin.Context, to string) {
	if g.GetHeader("HX-Request") != "" {
		g.Header("HX-Redirect", to)
		g.Status(http.StatusOK)
		g.Abort()
		return
	}
	g.Redirect(http.StatusFound, to)
	g.Abort()
}
