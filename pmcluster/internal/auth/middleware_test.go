package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeLookup is a test-local implementation of Lookup so we can control
// exactly what UserByToken returns without a real store.
type fakeLookup struct {
	fn func(ctx context.Context, token string) (*User, error)
}

func (f *fakeLookup) UserByToken(ctx context.Context, token string) (*User, error) {
	return f.fn(ctx, token)
}

// dummyHandler is a minimal http.Handler that records whether it was called
// and captures the user from context.
type dummyHandler struct {
	called bool
	user   *User
}

func (d *dummyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.called = true
	d.user = FromContext(r.Context())
	w.WriteHeader(http.StatusOK)
}

func applyBearer(lookup Lookup, req *http.Request) (*httptest.ResponseRecorder, *dummyHandler) {
	inner := &dummyHandler{}
	mw := Bearer(lookup)
	rec := httptest.NewRecorder()
	mw(inner).ServeHTTP(rec, req)
	return rec, inner
}

// TestBearer_MissingHeader verifies that a request with no Authorization header
// gets a 401 with WWW-Authenticate set.
func TestBearer_MissingHeader(t *testing.T) {
	lookup := &fakeLookup{fn: func(_ context.Context, _ string) (*User, error) {
		t.Error("lookup should not be called when header is missing")
		return nil, nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec, inner := applyBearer(lookup, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate header on 401")
	}
	if inner.called {
		t.Error("inner handler must not be called on missing header")
	}
}

// TestBearer_MalformedScheme verifies that "Basic ..." is rejected.
func TestBearer_MalformedScheme(t *testing.T) {
	lookup := &fakeLookup{fn: func(_ context.Context, _ string) (*User, error) {
		t.Error("lookup must not be called for non-Bearer scheme")
		return nil, nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec, inner := applyBearer(lookup, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if inner.called {
		t.Error("inner handler must not be called for Basic auth")
	}
}

// TestBearer_EmptyToken verifies that "Bearer " (empty token after scheme) is rejected.
func TestBearer_EmptyToken(t *testing.T) {
	lookup := &fakeLookup{fn: func(_ context.Context, _ string) (*User, error) {
		t.Error("lookup must not be called for empty token")
		return nil, nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec, inner := applyBearer(lookup, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if inner.called {
		t.Error("inner handler must not be called for empty token")
	}
}

// TestBearer_LookupReturnsError verifies that a lookup error results in 401
// (not 500 — errors are treated as authentication failures to avoid info leaks).
func TestBearer_LookupReturnsError(t *testing.T) {
	lookup := &fakeLookup{fn: func(_ context.Context, _ string) (*User, error) {
		return nil, errors.New("database unavailable")
	}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	rec, inner := applyBearer(lookup, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if inner.called {
		t.Error("inner handler must not be called when lookup errors")
	}
}

// TestBearer_LookupReturnsNilUser verifies that a nil user (valid token format,
// but no match in the store) results in 401.
func TestBearer_LookupReturnsNilUser(t *testing.T) {
	lookup := &fakeLookup{fn: func(_ context.Context, _ string) (*User, error) {
		return nil, nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer unknown-token")
	rec, inner := applyBearer(lookup, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if inner.called {
		t.Error("inner handler must not be called when user is nil")
	}
}

// TestBearer_ValidToken_ContextPropagation verifies that a successful lookup
// injects the user into the request context and the inner handler receives it
// via FromContext.
func TestBearer_ValidToken_ContextPropagation(t *testing.T) {
	want := &User{ID: 42, Name: "alice"}
	lookup := &fakeLookup{fn: func(_ context.Context, token string) (*User, error) {
		if token != "valid-token" {
			return nil, nil
		}
		return want, nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rec, inner := applyBearer(lookup, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !inner.called {
		t.Error("inner handler was not called for valid token")
	}
	if inner.user == nil {
		t.Fatal("FromContext returned nil inside inner handler")
	}
	if inner.user.ID != want.ID || inner.user.Name != want.Name {
		t.Errorf("context user = %+v, want %+v", inner.user, want)
	}
}

// TestBearer_CaseInsensitiveScheme verifies "bearer" (lowercase) is accepted.
func TestBearer_CaseInsensitiveScheme(t *testing.T) {
	want := &User{ID: 1, Name: "bob"}
	lookup := &fakeLookup{fn: func(_ context.Context, _ string) (*User, error) {
		return want, nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "bearer lower-case-token")
	rec, inner := applyBearer(lookup, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !inner.called {
		t.Error("inner handler was not called for lowercase bearer scheme")
	}
}

// TestFromContext_NoUser verifies that FromContext returns nil when no user
// has been injected into the context.
func TestFromContext_NoUser(t *testing.T) {
	ctx := context.Background()
	if u := FromContext(ctx); u != nil {
		t.Errorf("FromContext on empty context = %+v, want nil", u)
	}
}
