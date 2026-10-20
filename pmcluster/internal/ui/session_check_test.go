package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInternalSessionCheckFailsClosedAcrossNativeEdgeUpgrade(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()
	app := newTestApp(t, daemon)
	defer app.Close()

	check := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://edge_pmcluster-edge:8042/internal/session/check", nil)
		req.Host = "edge_pmcluster-edge:8042"
		req.Header.Set("X-Forwarded-Uri", path)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rr := httptest.NewRecorder()
		app.InternalSessionCheckHandler().ServeHTTP(rr, req)
		return rr
	}

	if got := check("/web/login", nil).Code; got != http.StatusNoContent {
		t.Fatalf("login check = %d, want 204", got)
	}
	unauth := check("/web/overview", nil)
	if unauth.Code != http.StatusFound || unauth.Header().Get("Location") != "/web/login" {
		t.Fatalf("unauth protected check = %d location %q", unauth.Code, unauth.Header().Get("Location"))
	}
	if got := check("/web/overview", nil); got.Code != http.StatusFound {
		t.Fatalf("repeated unauth protected check = %d, want redirect", got.Code)
	}

	jar := map[string]*http.Cookie{}
	doRequest(t, app, http.MethodPost, "/web/setup", "username=admin&password=supersecret&confirm=supersecret", jar)
	login := doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)
	if login.StatusCode != http.StatusFound {
		t.Fatalf("login = %d, want 302", login.StatusCode)
	}
	cookie := jar[app.Auth.CookieName()]
	if cookie == nil || strings.TrimSpace(cookie.Value) == "" {
		t.Fatal("login did not issue a session cookie")
	}
	if got := check("/web/overview", cookie).Code; got != http.StatusNoContent {
		t.Fatalf("authenticated protected check = %d, want 204", got)
	}

	logout := doRequest(t, app, http.MethodPost, "/web/logout", "", jar)
	if logout.StatusCode != http.StatusFound {
		t.Fatalf("logout = %d, want 302", logout.StatusCode)
	}
	if got := check("/web/overview", jar[app.Auth.CookieName()]).Code; got != http.StatusFound {
		t.Fatalf("post-logout protected check = %d, want redirect", got)
	}

	public := httptest.NewRequest(http.MethodGet, "http://pmcluster.example/internal/session/check", nil)
	public.Header.Set("X-Forwarded-Uri", "/web/overview")
	publicRR := httptest.NewRecorder()
	app.InternalSessionCheckHandler().ServeHTTP(publicRR, public)
	if publicRR.Code != http.StatusNotFound {
		t.Fatalf("public-host check = %d, want 404", publicRR.Code)
	}
}

func TestInternalSessionCheckRejectsLoginDisabledTask(t *testing.T) {
	daemon := fakeDaemon(t)
	defer daemon.Close()
	cfg := FromEnv()
	cfg.DataDir = t.TempDir()
	cfg.PMAPIURL = daemon.URL
	cfg.PMAPIToken = "pmc_test"
	cfg.SessionSecret = []byte("0123456789abcdefgh")
	cfg.CookieName = "pmui_session"
	cfg.LoginDisabled = true
	app, err := NewApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	req := httptest.NewRequest(http.MethodGet, "http://edge_pmcluster-edge:8042/internal/session/check", nil)
	req.Host = "edge_pmcluster-edge:8042"
	req.Header.Set("X-Forwarded-Uri", "/web/overview")
	rr := httptest.NewRecorder()
	app.InternalSessionCheckHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/web/login" {
		t.Fatalf("login-disabled check = %d location %q, want redirect", rr.Code, rr.Header().Get("Location"))
	}
}
