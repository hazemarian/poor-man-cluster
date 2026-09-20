package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

func requestWithUser(u *auth.User) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)

	ctx := req.Context()

	return req.WithContext(contextWithAuthUser(ctx, u))
}

// contextWithAuthUser injects a user into a context using the same key that
// auth.Bearer and auth.FromContext use. We have to route through Bearer
// middleware since ctxKey is unexported.
func contextWithAuthUser(ctx context.Context, u *auth.User) context.Context {

	var captured context.Context
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = r.Context()
	})
	middleware := auth.Bearer(staticLookup{u})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	if u != nil {
		req.Header.Set("Authorization", "Bearer dummy-token")
	}
	w := httptest.NewRecorder()
	middleware(inner).ServeHTTP(w, req)
	if captured != nil {
		return captured
	}
	return ctx
}

// staticLookup always returns the same user for any token.
type staticLookup struct{ user *auth.User }

func (s staticLookup) UserByToken(_ context.Context, _ string) (*auth.User, error) {
	return s.user, nil
}

// TestMe_AuthenticatedUser verifies Me returns 200 + correct JSON when a user
// is in the context (simulates Bearer middleware already having run).
func TestMe_AuthenticatedUser(t *testing.T) {
	u := &auth.User{ID: 7, Name: "testuser"}
	req := requestWithUser(u)
	rec := httptest.NewRecorder()

	Me(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["name"] != "testuser" {
		t.Errorf("name = %v, want testuser", body["name"])
	}
	if body["id"].(float64) != 7 {
		t.Errorf("id = %v, want 7", body["id"])
	}
}

// TestMe_MissingContextUser verifies the defensive 500 path when Me is called
// without Bearer middleware — i.e. auth.FromContext returns nil.
func TestMe_MissingContextUser(t *testing.T) {

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	rec := httptest.NewRecorder()

	Me(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
