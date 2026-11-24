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
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

const ctxUserKey = "pmui:user"

// WebBase is the URL prefix the operator console is mounted under
// (pmcluster.<domain>/web/*). Session redirects (setup nudges, login bounces)
// land under it so they stay inside Traefik's admin-auth gate.
const WebBase = "/web"

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
	// LoginDisabled no longer opens the console. It used to: every request ran as
	// a synthetic admin, on the reasoning that Traefik's admin-auth gate was in
	// front. That reasoning is exactly C1 of the 2026-11-21 audit — from inside
	// an exposed app, the gate is not there. The flag now only says "the password
	// form is not offered here" (an SSO-only console); identity and role always
	// come from a row in the user table.
	LoginDisabled bool
	// SSO reports that oauth2-proxy asserts identity through headers on every
	// request to the console. The proxy's org filter says who may reach the
	// console at all; the user table says what they may do once inside.
	SSO bool
}

// NewAuth wires session auth around the store. secret is the HMAC key.
func NewAuth(st *store.Store, secret []byte, cookieName string) *Auth {
	return &Auth{st: st, secret: secret, cookie: cookieName}
}

// CookieName returns the session cookie name.
func (a *Auth) CookieName() string { return a.cookie }

// TerminalTokenTTL bounds how long a terminal socket ticket is valid. The page mints it while
// rendering and connects immediately; two minutes covers a slow first paint, not a shared link.
const TerminalTokenTTL = 2 * time.Minute

// SignTerminalToken mints the ticket the interactive terminal carries into its websocket.
//
// Why a ticket at all: an interactive shell socket cannot ride a forward-auth gate. Traefik's
// forwardAuth re-issues the request to the auth server and, for a WebSocket upgrade, the browser's
// handshake never completes — the page loads, the socket dies, and the console never sees it
// (measured: two "page served" lines, zero "handshake arrived"). The console's /web router is
// therefore gated for pages only, and this socket proves itself: an HMAC over the stack, the
// service and an expiry, keyed with the console's own session secret. The page that mints it was
// gated; the ticket binds the socket to that page's stack and service and dies in two minutes.
func (a *Auth) SignTerminalToken(stack, service string, now time.Time) string {
	payload := fmt.Sprintf("%s|%s|%d", stack, service, now.Add(TerminalTokenTTL).Unix())
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte("terminal|" + payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyTerminalToken reports whether tok was minted for this stack+service and has not expired.
// A token for another stack, another service, or an earlier life is refused.
func (a *Auth) VerifyTerminalToken(tok, stack, service string, now time.Time) bool {
	body, sig, ok := strings.Cut(tok, ".")
	if !ok || body == "" || sig == "" {
		return false
	}
	rawSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	rawBody, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte("terminal|" + string(rawBody)))
	if !hmac.Equal(rawSig, mac.Sum(nil)) {
		return false
	}
	// The signed payload is authoritative, not the query: a caller cannot widen the scope of a
	// ticket it did not mint.
	gotStack, rest, ok := strings.Cut(string(rawBody), "|")
	if !ok {
		return false
	}
	gotService, expStr, ok := strings.Cut(rest, "|")
	if !ok {
		return false
	}
	if gotStack != stack || gotService != service {
		return false
	}
	deadline, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || now.After(time.Unix(deadline, 0)) {
		return false
	}
	return true
}

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
		Secure:   IsSecureRequest(c.Request),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

// IsSecureRequest reports whether the request arrived over HTTPS — either
// directly (a TLS connection) or through a proxy that set
// `X-Forwarded-Proto: https`. The session cookie's Secure flag is set from
// this so it is only ever sent over an encrypted channel.
func IsSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// ClearCookie expires the session cookie.
func (a *Auth) ClearCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: a.cookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
}

// CSRF returns gin middleware guarding state-changing console requests
// (POST/PUT/DELETE under /web/) against cross-site request forgery: the
// Origin (or Referer) host must match the request host. A missing Origin is
// allowed only when there is no session cookie — CLI/curl and the automated
// tests post without one — while a browser request that carries a session
// cookie but no Origin is refused. Mount it globally on the console engine.
func (a *Auth) CSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isStateChangingMethod(c.Request.Method) {
			c.Next()
			return
		}
		if !strings.HasPrefix(c.Request.URL.Path, WebBase+"/") {
			c.Next()
			return
		}

		origin := c.GetHeader("Origin")
		if origin == "" {
			origin = c.GetHeader("Referer")
		}
		if origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, c.Request.Host) {
				http.Error(c.Writer, "forbidden: cross-origin request", http.StatusForbidden)
				c.Abort()
				return
			}
			c.Next()
			return
		}

		// No Origin and no Referer: only a cookieless request is allowed
		// (curl/CLI/tests). A session cookie without an Origin is exactly the
		// forged cross-site POST CSRF guards against.
		if _, err := c.Cookie(a.cookie); err == nil {
			http.Error(c.Writer, "forbidden: missing origin", http.StatusForbidden)
			c.Abort()
			return
		}
		c.Next()
	}
}

// isStateChangingMethod reports whether the HTTP method mutates state.
func isStateChangingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// Require is gin middleware that allows only valid sessions. Unauthenticated
// requests redirect to /login (using HX-Redirect for HTMX partial loads so the
// SPA-style sidebar navigation lands on the login page). With LoginDisabled the
// gate is open: every request is treated as an authenticated admin.
func (a *Auth) Require() gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := a.identify(c)
		if !ok {
			// Under SSO there is nothing here to redirect TO: the sign-in page and
			// the setup form are the proxy's (they bounce), so a redirect becomes a
			// loop — /web/ -> /web/setup -> /web/ … Answering 401 is the honest
			// response, and it is what a request that did not come through the proxy
			// deserves.
			if a.SSO {
				c.String(http.StatusUnauthorized,
					"authentication required: this console signs in through the SSO proxy")
				c.Abort()
				return
			}
			a.redirect(c, a.target(c.Request.Context()))
			return
		}
		// A pending identity reaches the waiting page and nothing else: no
		// fragments, no API calls, no data. That is the whole point of the row
		// existing before an admin decides about it.
		if u.Status == store.StatusPending {
			if c.Request.URL.Path == WebBase+"/pending" || c.Request.URL.Path == WebBase+"/logout" {
				c.Set(ctxUserKey, u)
				c.Next()
				return
			}
			a.redirect(c, WebBase+"/pending")
			return
		}
		if u.Status == store.StatusDisabled {
			a.redirect(c, a.target(c.Request.Context()))
			return
		}

		// The setup nudge only makes sense where the password form exists; under SSO
		// the first admin is approved, not created here.
		if !a.SSO && !u.PasswordSet && u.AuthSource != store.AuthSSO && a.NudgeSetup != nil {
			if need, e := a.NudgeSetup(c.Request.Context()); e == nil && need {
				a.redirect(c, WebBase+"/setup")
				return
			}
		}
		c.Set(ctxUserKey, u)
		c.Next()
	}
}

// identify resolves the caller to a console user.
//
// Two sources, one authority afterwards. With SSO, oauth2-proxy has already
// authenticated the request and says who it is through headers; unknown
// identities get a pending row on first sight, so a stranger's login is a row to
// review, not access. Without SSO, the signed session cookie is the source, as
// before. Either way, the role and status read here are the row's.
func (a *Auth) identify(c *gin.Context) (*store.User, bool) {
	if a.SSO {
		email := strings.TrimSpace(c.GetHeader("X-Auth-Request-Email"))
		user := strings.TrimSpace(c.GetHeader("X-Auth-Request-User"))
		if email == "" && user == "" {
			return nil, false
		}
		if email == "" {
			email = user
		}
		if user == "" {
			user = email
		}
		u, err := a.st.EnsureSSOUser(c.Request.Context(), email, user)
		if err != nil || u == nil {
			// Silence here cost an afternoon: a console whose store could not be
			// written answered every SSO login with a redirect loop and logged
			// nothing at all. Say which identity failed and why.
			log.Printf("sso: cannot resolve identity %q: %v", email, err)
			return nil, false
		}
		return u, true
	}
	ck, err := c.Cookie(a.cookie)
	if err != nil {
		return nil, false
	}
	s, ok := a.Verify(ck)
	if !ok {
		return nil, false
	}
	u, err := a.st.ByUsername(c.Request.Context(), s.Username)
	if err != nil {
		return nil, false
	}
	return u, true
}

// roleRank returns the numeric precedence of a role (admin > operator >
// viewer). Unknown roles rank as viewer.
func roleRank(role string) int {
	switch role {
	case store.RoleAdmin:
		return 3
	case store.RoleOperator:
		return 2
	case store.RoleViewer:
		return 1
	}
	return 1
}

// RequireRole returns gin middleware that allows only sessions whose user holds
// at least the given role. It must run after Require (the user is read from
// context). Insufficient role yields a 403 (HX-Redirect to the overview for
// HTMX so the SPA doesn't get stuck on a forbidden fragment).
func (a *Auth) RequireRole(minRole string) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := CurrentUser(c)
		if u == nil || roleRank(u.Role) < roleRank(minRole) {
			if c.GetHeader("HX-Request") != "" {
				c.Header("HX-Redirect", WebBase+"/")
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			http.Error(c.Writer, "forbidden: insufficient role", http.StatusForbidden)
			c.Abort()
			return
		}
		c.Next()
	}
}

func (a *Auth) target(ctx context.Context) string {
	// There is no "no login" mode any more: LoginDisabled only hides the password
	// FORM on an SSO console (see LoginPage). Every unauthenticated request has to
	// land somewhere it can actually authenticate, or it bounces in a loop.
	if a.NudgeSetup != nil {
		if need, err := a.NudgeSetup(ctx); err == nil && need {
			return WebBase + "/setup"
		}
	}
	return WebBase + "/login"
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

// WithUser attaches u to the request the way Require does after a successful
// session check. It exists so fragments can be rendered in tests and tools
// without inventing a cookie, and keeps the context key private.
func WithUser(c *gin.Context, u *store.User) {
	c.Set(ctxUserKey, u)
}
