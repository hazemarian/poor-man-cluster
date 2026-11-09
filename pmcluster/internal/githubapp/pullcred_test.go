package githubapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestPullCredentialRouteMatchesItsConstant: the route is registered with a
// literal (docscheck reads the AST) and the edge calls the constant, so the two
// must not drift.
func TestPullCredentialRouteMatchesItsConstant(t *testing.T) {
	r := chi.NewRouter()
	(&HTTP{}).Mount(r)
	found := false
	for _, rt := range r.Routes() {
		if strings.HasSuffix(rt.Pattern, PullCredentialPath) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no POST route ending in %s is registered; routes: %+v", PullCredentialPath, r.Routes())
	}
}

// TestPullCredentialWithoutAnAppNamesTheFix: the daemon half must answer the
// edge with a sentence an operator can act on, and never with a token.
func TestPullCredentialWithoutAnAppNamesTheFix(t *testing.T) {
	r := chi.NewRouter()
	(&HTTP{}).Mount(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/github/pull-credential", strings.NewReader(`{"server":"ghcr.io"}`)))
	if rec.Code == http.StatusOK {
		t.Fatal("a daemon with no app answered 200")
	}
	if !strings.Contains(rec.Body.String(), "pmcluster github app set") {
		t.Errorf("body = %q, want the command that configures an app", rec.Body.String())
	}
}
