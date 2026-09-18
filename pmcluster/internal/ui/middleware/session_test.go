package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/store"
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
