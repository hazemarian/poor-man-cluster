package middleware

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// newAuth builds an Auth over an in-memory store with no users (RequireRole is
// tested with synthetic context users, so the store is only used for its type).
func newAuth(t *testing.T) *Auth {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewAuth(st, []byte("0123456789abcdefgh"), "pmui_session")
}

func TestRequireRole_AdminPasses(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", a.RequireRole(store.RoleAdmin), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Request = req
	// Simulate Require() having set the user.
	c.Set(ctxUserKey, &store.User{Role: store.RoleAdmin})
	a.RequireRole(store.RoleAdmin)(c)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin request = %d, want 200", rr.Code)
	}
}

func TestRequireRole_OperatorBlockedFromAdmin(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/admin", a.RequireRole(store.RoleAdmin), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Request = req
	c.Set(ctxUserKey, &store.User{Role: store.RoleOperator})
	a.RequireRole(store.RoleAdmin)(c)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("operator on admin route = %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "forbidden: insufficient role") {
		t.Errorf("body = %q, want forbidden message", rr.Body.String())
	}
}

func TestRequireRole_ViewerBlockedFromOperator(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/mutate", a.RequireRole(store.RoleOperator), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodPost, "/mutate", nil)
	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Request = req
	c.Set(ctxUserKey, &store.User{Role: store.RoleViewer})
	a.RequireRole(store.RoleOperator)(c)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer on operator route = %d, want 403", rr.Code)
	}
}

func TestRequireRole_HTMXGetsRedirectHeader(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/mutate", a.RequireRole(store.RoleOperator), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodPost, "/mutate", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Request = req
	c.Set(ctxUserKey, &store.User{Role: store.RoleViewer})
	a.RequireRole(store.RoleOperator)(c)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("HTMX viewer on operator route = %d, want 403", rr.Code)
	}
	if got := rr.Header().Get("HX-Redirect"); got != "/web/" {
		t.Errorf("HX-Redirect = %q, want /web/", got)
	}
}

func TestRequireRole_NoUserBlocked(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", a.RequireRole(store.RoleViewer), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Request = req
	a.RequireRole(store.RoleViewer)(c)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("no-user request = %d, want 403", rr.Code)
	}
}

func TestRequire_LoginDisabledPassThrough(t *testing.T) {
	a := newAuth(t)
	a.LoginDisabled = true
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", a.Require(), func(c *gin.Context) {
		u := CurrentUser(c)
		if u == nil {
			c.String(http.StatusInternalServerError, "no user")
			return
		}
		c.String(http.StatusOK, u.Username+":"+u.Role)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("login-disabled request = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "admin:admin" {
		t.Errorf("synthetic user = %q, want admin:admin", got)
	}
}

// TestVerify_RoundTrip checks a freshly signed cookie verifies back to the
// same session.
func TestVerify_RoundTrip(t *testing.T) {
	a := newAuth(t)
	v := a.sign(Session{Username: "alice", Exp: time.Now().Add(time.Hour).Unix()})
	s, ok := a.Verify(v)
	if !ok {
		t.Fatal("Verify(valid) = false, want true")
	}
	if s.Username != "alice" {
		t.Errorf("username = %q, want alice", s.Username)
	}
}

func TestVerify_RejectsInvalid(t *testing.T) {
	a := newAuth(t)

	// Empty, malformed, tampered payload, tampered signature, garbage.
	for _, v := range []string{
		"",
		"abc",
		"a.b.c",
		"abc.def",
		a.sign(Session{Username: "alice", Exp: time.Now().Add(time.Hour).Unix()}) + "x",
		"!!.!!",
	} {
		if _, ok := a.Verify(v); ok {
			t.Errorf("Verify(%q) = true, want false", v)
		}
	}
}

func TestVerify_RejectsExpired(t *testing.T) {
	a := newAuth(t)
	v := a.sign(Session{Username: "alice", Exp: time.Now().Add(-time.Minute).Unix()})
	if _, ok := a.Verify(v); ok {
		t.Error("Verify(expired) = true, want false")
	}
}

func TestVerify_RejectsSignatureFromOtherKey(t *testing.T) {
	a := newAuth(t)
	other := NewAuth(a.st, []byte("different-secret-0123456789"), "pmui_session")
	v := other.sign(Session{Username: "alice", Exp: time.Now().Add(time.Hour).Unix()})
	if _, ok := a.Verify(v); ok {
		t.Error("Verify(cookie signed by other key) = true, want false")
	}
}

// TestSetCookie_SetsSignedCookie checks SetCookie emits a HttpOnly cookie that
// Verify accepts.
func TestSetCookie_SetsSignedCookie(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/login", func(c *gin.Context) {
		a.SetCookie(c, "alice", time.Hour)
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	cookies := rr.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cookies))
	}
	ck := cookies[0]
	if !ck.HttpOnly {
		t.Error("cookie not HttpOnly")
	}
	if _, ok := a.Verify(ck.Value); !ok {
		t.Error("Verify(set cookie) = false, want true")
	}
}

// TestSetCookie_SecureFlag checks the session cookie's Secure flag follows the
// request scheme: set over TLS or X-Forwarded-Proto: https, unset over plain
// HTTP — while HttpOnly + SameSite=Lax stay fixed.
func TestSetCookie_SecureFlag(t *testing.T) {
	a := newAuth(t)
	cases := []struct {
		name     string
		tls      bool
		proto    string
		secure   bool
		sameSite http.SameSite
	}{
		{"plain http", false, "", false, http.SameSiteLaxMode},
		{"tls", true, "", true, http.SameSiteLaxMode},
		{"forwarded https", false, "https", true, http.SameSiteLaxMode},
		{"forwarded http", false, "http", false, http.SameSiteLaxMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.GET("/login", func(c *gin.Context) {
				a.SetCookie(c, "alice", time.Hour)
				c.Status(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodGet, "/login", nil)
			req.TLS = nil
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			if tc.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			cookies := rr.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("cookies = %d, want 1", len(cookies))
			}
			ck := cookies[0]
			if ck.Secure != tc.secure {
				t.Errorf("Secure = %v, want %v", ck.Secure, tc.secure)
			}
			if !ck.HttpOnly {
				t.Error("HttpOnly must stay true")
			}
			if ck.SameSite != tc.sameSite {
				t.Errorf("SameSite = %v, want %v", ck.SameSite, tc.sameSite)
			}
		})
	}
}

// TestIsSecureRequest covers the scheme detection helper directly, including
// a comma-separated X-Forwarded-Proto list where the first hop decides.
func TestIsSecureRequest(t *testing.T) {
	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	if IsSecureRequest(plain) {
		t.Error("plain HTTP request reported secure")
	}
	https := httptest.NewRequest(http.MethodGet, "/", nil)
	https.Header.Set("X-Forwarded-Proto", "https")
	if !IsSecureRequest(https) {
		t.Error("X-Forwarded-Proto: https reported insecure")
	}
	list := httptest.NewRequest(http.MethodGet, "/", nil)
	list.Header.Set("X-Forwarded-Proto", "https, http")
	if !IsSecureRequest(list) {
		t.Error("X-Forwarded-Proto: 'https, http' should read as https (first hop)")
	}
	tlsReq := httptest.NewRequest(http.MethodGet, "/", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	if !IsSecureRequest(tlsReq) {
		t.Error("TLS request reported insecure")
	}
}

// TestClearCookie_ExpiresImmediately checks ClearCookie issues an expired
// cookie value.
func TestClearCookie_ExpiresImmediately(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/logout", func(c *gin.Context) {
		a.ClearCookie(c)
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/logout", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	cookies := rr.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != "" || cookies[0].MaxAge != -1 {
		t.Errorf("clear cookie = %+v, want empty value + MaxAge -1", cookies)
	}
}

// TestRequire_RedirectsToLogin checks Require without a valid cookie lands on
// /web/login.
func TestRequire_RedirectsToLogin(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", a.Require(), func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("no-cookie request = %d, want 302", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/web/login" {
		t.Errorf("redirect = %q, want /web/login", loc)
	}
}

// TestRequire_HTMXRedirectUnauthorized checks HTMX requests get HX-Redirect
// instead of a Location redirect when unauthenticated.
func TestRequire_HTMXRedirectUnauthorized(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", a.Require(), func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("htmx no-cookie = %d, want 401", rr.Code)
	}
	if got := rr.Header().Get("HX-Redirect"); got != "/web/login" {
		t.Errorf("HX-Redirect = %q, want /web/login", got)
	}
}

// TestRequire_TamperedCookieRedirects checks a cookie with a valid shape but
// wrong signature redirects (no crash).
func TestRequire_TamperedCookieRedirects(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", a.Require(), func(c *gin.Context) { c.Status(http.StatusOK) })

	good := a.sign(Session{Username: "alice", Exp: time.Now().Add(time.Hour).Unix()})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: a.cookie, Value: good + "x"})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("tampered cookie = %d, want 302", rr.Code)
	}
}

// TestCSRF_RejectsForeignOrigin verifies a state-changing console request with
// a mismatched Origin is refused 403, while a same-origin request passes.
func TestCSRF_RejectsForeignOrigin(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(a.CSRF())
	r.POST("/web/stacks/x/sync", func(c *gin.Context) { c.Status(http.StatusOK) })

	// Foreign origin → 403.
	req := httptest.NewRequest(http.MethodPost, "http://pm.example.com/web/stacks/x/sync", nil)
	req.Header.Set("Origin", "https://evil.example.net")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("foreign origin = %d, want 403", rr.Code)
	}

	// Same host → passes.
	req = httptest.NewRequest(http.MethodPost, "http://pm.example.com/web/stacks/x/sync", nil)
	req.Header.Set("Origin", "http://pm.example.com")
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("same origin = %d, want 200", rr.Code)
	}
}

// TestCSRF_MissingOriginAllowedOnlyWithoutSessionCookie verifies a cookieless
// POST without an Origin (curl/CLI/tests) passes, but the same POST carrying a
// session cookie is refused.
func TestCSRF_MissingOriginAllowedOnlyWithoutSessionCookie(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(a.CSRF())
	r.POST("/web/stacks/x/sync", func(c *gin.Context) { c.Status(http.StatusOK) })

	// No Origin, no session cookie → allowed.
	req := httptest.NewRequest(http.MethodPost, "http://pm.example.com/web/stacks/x/sync", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("cookieless no-origin = %d, want 200", rr.Code)
	}

	// Session cookie present but no Origin → 403.
	req = httptest.NewRequest(http.MethodPost, "http://pm.example.com/web/stacks/x/sync", nil)
	req.AddCookie(&http.Cookie{Name: a.cookie, Value: a.sign(Session{Username: "alice", Exp: time.Now().Add(time.Hour).Unix()})})
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("session cookie no-origin = %d, want 403", rr.Code)
	}
}

// TestCSRF_SkipsNonWebAndReadOnly verifies GETs and non-/web/ paths are never
// gated by the CSRF middleware.
func TestCSRF_SkipsNonWebAndReadOnly(t *testing.T) {
	a := newAuth(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(a.CSRF())
	r.GET("/web/stacks", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/api/whatever", func(c *gin.Context) { c.Status(http.StatusOK) })

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/web/stacks"},
		{http.MethodPost, "/api/whatever"},
	} {
		req := httptest.NewRequest(tc.method, "http://pm.example.com"+tc.path, nil)
		req.Header.Set("Origin", "https://evil.example.net")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("%s %s = %d, want 200 (must skip CSRF)", tc.method, tc.path, rr.Code)
		}
	}
}

// TestRoleRank_UnknownRanksAsViewer checks roleRank falls back to viewer.
func TestRoleRank_UnknownRanksAsViewer(t *testing.T) {
	if got := roleRank("superadmin"); got != 1 {
		t.Errorf("roleRank(superadmin) = %d, want 1 (viewer)", got)
	}
	if got := roleRank(store.RoleAdmin); got != 3 {
		t.Errorf("roleRank(admin) = %d, want 3", got)
	}
	if got := roleRank(store.RoleOperator); got != 2 {
		t.Errorf("roleRank(operator) = %d, want 2", got)
	}
	if got := roleRank(store.RoleViewer); got != 1 {
		t.Errorf("roleRank(viewer) = %d, want 1", got)
	}
}

// TestCurrentUser_NoValue verifies CurrentUser on a bare context returns nil.
func TestCurrentUser_NoValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	if u := CurrentUser(c); u != nil {
		t.Errorf("CurrentUser = %+v, want nil", u)
	}
}
