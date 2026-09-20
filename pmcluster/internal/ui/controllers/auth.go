package controllers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// Auth handles sign-in, the first-run password setup, and sign-out.
type Auth struct{ *Controller }

// needSetup reports whether a first admin still needs to be created (first run:
// zero users, or the bootstrap admin has no password yet).
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

// LoginPage renders the sign-in form. When EDGE_LOGIN_DISABLED is set the
// console is fully open (Traefik admin-auth gates it), so the form bounces.
func (c Auth) LoginPage(g *gin.Context) {
	if c.Auth.LoginDisabled {
		redirect(g, WebBase+"/")
		return
	}
	c.Views.Page(g, "login", loginData{Version: c.Version})
}

// Login verifies credentials and issues a session cookie.
func (c Auth) Login(g *gin.Context) {
	if c.Auth.LoginDisabled {
		redirect(g, WebBase+"/")
		return
	}
	username := g.PostForm("username")
	password := g.PostForm("password")

	u, err := c.Store.GetByUsername(g.Request.Context(), username)
	if err != nil || !u.PasswordSet || !checkPassword(u, password) {
		c.Views.Page(g, "login", loginData{Version: c.Version, Error: "Invalid username or password."})
		return
	}
	c.Auth.SetCookie(g, u.Username, 7*24*time.Hour)
	redirect(g, WebBase+"/")
}

// SetupPage renders the first-run create-admin form. It only makes sense while
// no admin exists yet; otherwise bounce to login (or /web/ when login is
// disabled).
func (c Auth) SetupPage(g *gin.Context) {
	if c.Auth.LoginDisabled {
		redirect(g, WebBase+"/")
		return
	}
	need, err := c.needSetup(g.Request.Context())
	if err != nil || !need {
		redirect(g, WebBase+"/login")
		return
	}
	c.Views.Page(g, "setup", gin.H{})
}

// Setup creates the first admin account on first run (username + password).
func (c Auth) Setup(g *gin.Context) {
	if c.Auth.LoginDisabled {
		redirect(g, WebBase+"/")
		return
	}
	need, err := c.needSetup(g.Request.Context())
	if err != nil || !need {
		redirect(g, WebBase+"/login")
		return
	}
	uname := strings.TrimSpace(g.PostForm("username"))
	pass := g.PostForm("password")
	confirm := g.PostForm("confirm")
	if uname == "" || len(pass) < 8 || pass != confirm {
		c.Views.Page(g, "setup", gin.H{
			"Error":   "Username is required; password must be at least 8 characters and match the confirmation.",
			"Payload": gin.H{"Username": uname},
		})
		return
	}
	if _, err := c.Store.CreateUser(g.Request.Context(), uname, hashPassword(pass), true, store.RoleAdmin); err != nil {
		c.Views.Page(g, "setup", gin.H{"Error": "Could not create admin: " + err.Error()})
		return
	}
	c.Auth.SetCookie(g, uname, 7*24*time.Hour)
	c.Views.Page(g, "setup", gin.H{"Username": uname, "Done": true})
}

// Logout clears the session cookie and returns to the sign-in screen (or the
// console root when login is disabled).
func (c Auth) Logout(g *gin.Context) {
	c.Auth.ClearCookie(g)
	if c.Auth.LoginDisabled {
		redirect(g, WebBase+"/")
		return
	}
	redirect(g, WebBase+"/login")
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
