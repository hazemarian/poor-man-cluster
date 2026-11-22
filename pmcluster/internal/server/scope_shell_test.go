package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

// The bug these tests pin: stackScopeGuard runs on the whole /api subtree and fails closed whenever
// it cannot parse a stack out of the path. The shell routes name a session, not a stack, so a scoped
// token could open a shell and then never close it — the session outlived the operator, holding the
// stack's secrets, until its 30-minute ceiling. The handler's own guard (authorizeShellTarget) is a
// stricter check, so the middleware hands these two prefixes to it.

// scopedDeleteRequest drives the REAL chain the server mounts: bearer, then the scope guard, then the
// shell handler. The earlier tests called authorizeShellTarget directly, which is exactly how the
// middleware in front of it escaped notice.
func scopedDeleteRequest(t *testing.T, d Deps, path string) *httptest.ResponseRecorder {
	t.Helper()
	h := auth.Bearer(d.Lookup)(stackScopeGuard(ShellRemoveHandler(d)))
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestScopeGuard_LetsAScopedTokenCloseItsOwnShell is the regression: the shell exists, the token is
// scoped to the stack that owns it, and closing it must succeed.
func TestScopeGuard_LetsAScopedTokenCloseItsOwnShell(t *testing.T) {
	closed := ""
	d := Deps{
		Lookup: scopeLookup{user: &auth.User{Name: "ci", Stack: "team-a"}},
		Docker: scopeFixture(),
		CloseShell: func(_ context.Context, id string) error {
			closed = id
			return nil
		},
	}
	rec := scopedDeleteRequest(t, d, "/api/shells/pmshell-1")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: a token scoped to team-a could not close team-a's own shell (%s)",
			rec.Code, rec.Body.String())
	}
	if closed != "pmshell-1" {
		t.Fatalf("CloseShell called with %q, want pmshell-1", closed)
	}
}

// TestScopeGuard_StillRefusesAnotherStacksShell — handing the route to the handler must not weaken the
// scope: the handler compares the session's own stack label against the token.
func TestScopeGuard_StillRefusesAnotherStacksShell(t *testing.T) {
	d := Deps{
		Lookup:     scopeLookup{user: &auth.User{Name: "ci", Stack: "team-b"}},
		Docker:     scopeFixture(),
		CloseShell: func(context.Context, string) error { return nil },
	}
	rec := scopedDeleteRequest(t, d, "/api/shells/pmshell-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: team-b's token closed team-a's shell", rec.Code)
	}
}

// TestScopeGuard_ShellRouteStillRefusesANonShellService — the exemption is by prefix, not by token, so
// the audit's original target (the service that mounts the Docker socket) stays out of reach.
func TestScopeGuard_ShellRouteStillRefusesANonShellService(t *testing.T) {
	d := Deps{
		Lookup:     scopeLookup{user: &auth.User{Name: "ci", Stack: "team-a"}},
		Docker:     scopeFixture(),
		CloseShell: func(context.Context, string) error { return nil },
	}
	rec := scopedDeleteRequest(t, d, "/api/shells/infra_traefik")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a scoped token removed a non-shell service", rec.Code)
	}
}

// TestScopeGuard_StillFailsClosedOnOtherAPIRoutes — the exemption is two prefixes wide, nothing more.
// /api/me, settings, backups et al. remain refused for a scoped token.
func TestScopeGuard_StillFailsClosedOnOtherAPIRoutes(t *testing.T) {
	d := Deps{Lookup: scopeLookup{user: &auth.User{Name: "ci", Stack: "team-a"}}}
	for _, path := range []string{"/api/me", "/api/settings", "/api/backups", "/api/cluster/info"} {
		h := auth.Bearer(d.Lookup)(stackScopeGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Errorf("%s reached its handler; a scoped token must be refused before it", path)
			w.WriteHeader(http.StatusOK)
		})))
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, rec.Code)
		}
	}
}

// TestScopeGuard_ExecWSPrefixReachesItsHandler — /api/shell/{id}/exec/ws is the other shell route; it
// must reach authorizeShellTarget rather than being refused for its shape.
func TestScopeGuard_ExecWSPrefixReachesItsHandler(t *testing.T) {
	reached := false
	d := Deps{Lookup: scopeLookup{user: &auth.User{Name: "ci", Stack: "team-a"}}}
	h := auth.Bearer(d.Lookup)(stackScopeGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/api/shell/pmshell-1/exec/ws", nil)
	req.Header.Set("Authorization", "Bearer tok")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !reached {
		t.Fatal("the scope guard refused the exec-ws route before its handler could check the target")
	}
}
