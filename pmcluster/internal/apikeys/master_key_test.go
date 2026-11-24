package apikeys

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

// fixedLookup resolves any token to one principal, so the tests exercise the REAL
// bearer middleware instead of hand-injecting a context value.
type fixedLookup struct{ user *auth.User }

func (f fixedLookup) UserByToken(context.Context, string) (*auth.User, error) { return f.user, nil }

// asUser runs a request through the chain the server actually mounts — bearer,
// then the create handler — so the admin guard is tested where it runs.
func asUser(t *testing.T, svc Service, caller *auth.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	h := HTTP{Svc: svc}
	r.Post("/api_keys", auth.Bearer(fixedLookup{user: caller})(http.HandlerFunc(h.create)).ServeHTTP)
	req := httptest.NewRequest(http.MethodPost, "/api_keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestMasterKeyRequiresAnAdminCaller is the guard the UI depends on: a master
// (admin) key must not be mintable by an operator token, or an operator could
// escalate itself through the key API in one request.
func TestMasterKeyRequiresAnAdminCaller(t *testing.T) {
	st := openTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	svc := NewLocal(st)

	rec := asUser(t, svc, &auth.User{Name: "ci", Role: auth.RoleOperator},
		`{"name":"master","role":"admin"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator minting an admin key = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	rec = asUser(t, svc, &auth.User{Name: "edge", Role: auth.RoleAdmin}, `{"name":"master","role":"admin"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin minting an admin key = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created["role"] != "admin" {
		t.Errorf("response role = %v, want admin", created["role"])
	}

	keys, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 || keys[0].Role != auth.RoleAdmin || keys[0].Kind != KindAPI {
		t.Fatalf("stored key = %+v, want one admin/api key", keys)
	}
}

// TestCreateDefaultsStayLeastPrivileged — a create that asks for nothing must not
// hand out a master key: operator tier, api surface.
func TestCreateDefaultsStayLeastPrivileged(t *testing.T) {
	st := openTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	svc := NewLocal(st)

	if rec := asUser(t, svc, &auth.User{Name: "op", Role: auth.RoleOperator}, `{"name":"plain"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	keys, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if keys[0].Role != auth.RoleOperator || keys[0].Kind != KindAPI {
		t.Fatalf("default key = %+v, want operator/api", keys[0])
	}
}

// TestCreateRejectsUnknownRoleAndKind — a typo must be a 400, not a silent
// downgrade or upgrade.
func TestCreateRejectsUnknownRoleAndKind(t *testing.T) {
	st := openTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	svc := NewLocal(st)
	admin := &auth.User{Name: "edge", Role: auth.RoleAdmin}

	for _, body := range []string{
		`{"name":"a","role":"superuser"}`,
		`{"name":"b","kind":"grpc"}`,
	} {
		if rec := asUser(t, svc, admin, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
	}
}

// TestMCPKeyIsStoredAsSuch — the surface travels to the store, because that is
// what the REST guard reads on every later request.
func TestMCPKeyIsStoredAsSuch(t *testing.T) {
	st := openTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	svc := NewLocal(st)

	if rec := asUser(t, svc, &auth.User{Name: "edge", Role: auth.RoleAdmin}, `{"name":"agent","kind":"mcp"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create mcp = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	keys, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if keys[0].Kind != KindMCP {
		t.Fatalf("kind = %q, want mcp", keys[0].Kind)
	}
}
