// Package middleware provides session auth for the UI.
//
// Sessions are stateless HMAC-signed cookies carrying the username and an
// expiry; the user row is re-read from the store on each request so a password
// change or removal takes effect immediately.
package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/store"
)

const ctxUserKey = "pmui:user"

// Session is the signed payload stored in the cookie.
type Session struct {
	Username string `json:"u"`
	Exp      int64  `json:"exp"`
}

// Auth issues and verifies session cookies and gates protected routes.
type Auth struct {
	st     *store.Store
	secret []byte
	cookie string
	// setupRequired returns true while the bootstrap admin has no password yet.
	NudgeSetup func(c context.Context) (bool, error)
}

// NewAuth wires session auth around the store. secret is the HMAC key.
func NewAuth(st *store.Store, secret []byte, cookieName string) *Auth {
	return &Auth{st: st, secret: secret, cookie: cookieName}
}

// CookieName returns the session cookie name.
func (a *Auth) CookieName() string { return a.cookie }

func (a *Auth) sign(s Session) string {
	raw, _ := json.Marshal(s)
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return body + "." + sig
}

// Verify validates a signed cookie value and returns the session. An empty
// value or bad signature / expired token yields ok=false.
func (a *Auth) Verify(v string) (Session, bool) {
	if v == "" {
		return Session{}, false
	}
	parts := strings.Split(v, ".")
	if len(parts) != 2 {
		return Session{}, false
	}
	want := hmac.New(sha256.New, a.secret)
	want.Write([]byte(parts[0]))
	got, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(want.Sum(nil), got) {
		return Session{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Session{}, false
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return Session{}, false
	}
	if time.Now().Unix() > s.Exp {
		return Session{}, false
	}
	return s, true
}

// SetCookie issues a session cookie for username for ttl.
func (a *Auth) SetCookie(c *gin.Context, username string, ttl time.Duration) {
	s := Session{Username: username, Exp: time.Now().Add(ttl).Unix()}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     a.cookie,
		Value:    a.sign(s),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

// ClearCookie expires the session cookie.
func (a *Auth) ClearCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: a.cookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
}

// Require is gin middleware that allows only valid sessions. Unauthenticated
// requests redirect to /login (using HX-Redirect for HTMX partial loads so the
// SPA-style sidebar navigation lands on the login page).
func (a *Auth) Require() gin.HandlerFunc {
	return func(c *gin.Context) {
		ck, err := c.Cookie(a.cookie)
		if err != nil {
			a.redirect(c, a.target(c.Request.Context()))
			return
		}
		s, ok := a.Verify(ck)
		if !ok {
			a.redirect(c, a.target(c.Request.Context()))
			return
		}
		u, err := a.st.GetByUsername(c.Request.Context(), s.Username)
		if err != nil {
			a.redirect(c, a.target(c.Request.Context()))
			return
		}

		if !u.PasswordSet && a.NudgeSetup != nil {
			if need, e := a.NudgeSetup(c.Request.Context()); e == nil && need {
				a.redirect(c, "/setup")
				return
			}
		}
		c.Set(ctxUserKey, u)
		c.Next()
	}
}

// target picks where an unauthenticated visitor should land: the first-run
// setup (nudge a password) when the bootstrap admin has none, else the login.
func (a *Auth) target(ctx context.Context) string {
	if a.NudgeSetup != nil {
		if need, err := a.NudgeSetup(ctx); err == nil && need {
			return "/setup"
		}
	}
	return "/login"
}

func (a *Auth) redirect(c *gin.Context, to string) {
	if c.GetHeader("HX-Request") != "" {
		c.Header("HX-Redirect", to)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	c.Redirect(http.StatusFound, to)
	c.Abort()
}

// CurrentUser returns the logged-in user set by Require, or nil.
func CurrentUser(c *gin.Context) *store.User {
	v, _ := c.Get(ctxUserKey)
	u, _ := v.(*store.User)
	return u
}
