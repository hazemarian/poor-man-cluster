package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/apikeys"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

type kindLookup struct{ user *auth.User }

func (f kindLookup) UserByToken(context.Context, string) (*auth.User, error) { return f.user, nil }

func kindRequest(t *testing.T, u *auth.User, path string) int {
	t.Helper()
	reached := false
	h := auth.Bearer(kindLookup{user: u})(refuseMCPKeys(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK && !reached {
		t.Fatal("200 without reaching the handler")
	}
	return rec.Code
}

// TestMCPKeyIsRefusedOnTheRESTSurface is the whole point of the key kind: an AI
// client's key reads logs and deploys through MCP tools; it must not also be able
// to read settings, secrets or rendered configs.
func TestMCPKeyIsRefusedOnTheRESTSurface(t *testing.T) {
	u := &auth.User{Name: "agent", Role: auth.RoleAdmin, Kind: apikeys.KindMCP}
	for _, path := range []string{"/api/me", "/api/settings", "/api/secrets", "/api/stacks/demo", "/api/api_keys"} {
		if code := kindRequest(t, u, path); code != http.StatusForbidden {
			t.Errorf("mcp key on %s = %d, want 403", path, code)
		}
	}
}

// TestAPIKeyReachesTheRESTSurface — the guard must be specific to the MCP kind, and
// an empty kind (every pre-existing token, read from a row that predates the
// column) must behave exactly as before.
func TestAPIKeyReachesTheRESTSurface(t *testing.T) {
	for _, kind := range []string{"", apikeys.KindAPI} {
		u := &auth.User{Name: "ci", Role: auth.RoleOperator, Kind: kind}
		if code := kindRequest(t, u, "/api/me"); code != http.StatusOK {
			t.Errorf("kind %q on /api/me = %d, want 200", kind, code)
		}
	}
}
