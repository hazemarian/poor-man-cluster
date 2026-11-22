package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// scopeLookup maps one token to one principal, so the tests exercise the REAL bearer middleware
// instead of injecting a user into the context by hand.
type scopeLookup struct{ user *auth.User }

func (f scopeLookup) UserByToken(context.Context, string) (*auth.User, error) { return f.user, nil }

// scopeDocker answers ServiceInspect from a fixture and records what it was asked for.
type scopeDocker struct {
	runtime.Client
	svcs map[string]runtime.ServiceInspectResult
}

func (d scopeDocker) ServiceInspect(_ context.Context, name string) (runtime.ServiceInspectResult, error) {
	svc, ok := d.svcs[name]
	if !ok {
		return runtime.ServiceInspectResult{}, context.DeadlineExceeded // any error: "no such service"
	}
	return svc, nil
}

func scopeFixture() scopeDocker {
	return scopeDocker{svcs: map[string]runtime.ServiceInspectResult{
		// a real shell session belonging to stack "team-a"
		"pmshell-1": {ID: "pmshell-1", Name: "pmshell-team-a-111", Labels: map[string]string{
			runtime.ShellLabel:      "true",
			runtime.ShellStackLabel: "team-a",
		}},
		// the service that mounts the Docker socket — the audit's concrete target
		"infra_traefik": {ID: "infra_traefik", Name: "infra_traefik", Labels: map[string]string{}},
	}}
}

func guardedRequest(t *testing.T, d Deps, method, path, id string) *httptest.ResponseRecorder {
	t.Helper()
	h := auth.Bearer(d.Lookup)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorizeShellTarget(w, r, d, id) {
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAuthorizeShellTarget_RefusesANonShellService is the finding itself: a valid token naming
// infra_traefik must not get a container back, because that exec is root on the manager.
func TestAuthorizeShellTarget_RefusesANonShellService(t *testing.T) {
	d := Deps{Lookup: scopeLookup{user: &auth.User{Name: "op"}}, Docker: scopeFixture()}
	rec := guardedRequest(t, d, http.MethodGet, "/api/shell/infra_traefik/exec/ws", "infra_traefik")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a token must not exec into a service that is not a shell session", rec.Code)
	}
}

// TestAuthorizeShellTarget_RefusesAnotherStacksShell — a stack-scoped token may not reach a shell that
// belongs to a different stack, even one that is legitimately a shell session.
func TestAuthorizeShellTarget_RefusesAnotherStacksShell(t *testing.T) {
	d := Deps{
		Lookup: scopeLookup{user: &auth.User{Name: "ci", Stack: "team-b"}},
		Docker: scopeFixture(),
	}
	rec := guardedRequest(t, d, http.MethodDelete, "/api/shells/pmshell-1", "pmshell-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: team-b's token reached team-a's shell", rec.Code)
	}
}

// TestAuthorizeShellTarget_AllowsItsOwnShell — the guard must not break the feature it protects.
func TestAuthorizeShellTarget_AllowsItsOwnShell(t *testing.T) {
	for _, u := range []*auth.User{
		{Name: "op"},                  // unscoped: any shell session
		{Name: "ci", Stack: "team-a"}, // scoped to the owning stack
	} {
		d := Deps{Lookup: scopeLookup{user: u}, Docker: scopeFixture()}
		rec := guardedRequest(t, d, http.MethodGet, "/api/shell/pmshell-1/exec/ws", "pmshell-1")
		if rec.Code != http.StatusOK {
			t.Errorf("user %+v: status = %d, want 200 — the guard broke a legitimate session", u, rec.Code)
		}
	}
}

// TestAuthorizeShellTarget_UnknownIdIs404 — an id that does not resolve is a 404, not a 403: the two
// mean different things to whoever is holding the token.
func TestAuthorizeShellTarget_UnknownIdIs404(t *testing.T) {
	d := Deps{Lookup: scopeLookup{user: &auth.User{Name: "op"}}, Docker: scopeFixture()}
	rec := guardedRequest(t, d, http.MethodGet, "/api/shell/nope/exec/ws", "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
