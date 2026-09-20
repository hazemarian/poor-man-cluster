package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

// fakeLookup is the minimal auth.Lookup for integration tests — no DB needed.
type fakeLookup struct {
	users map[string]*auth.User
}

func (f *fakeLookup) UserByToken(_ context.Context, token string) (*auth.User, error) {
	return f.users[token], nil
}

// TestRoutes_Health verifies /health is unauthenticated and returns 200/JSON.
// Sub-agent (Phase 1.7) should expand: assert version field, headers, etc.
func TestRoutes_Health(t *testing.T) {
	srv := httptest.NewServer(New(Deps{Lookup: &fakeLookup{}}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("body.status = %v, want ok", body["status"])
	}
}

// TestRoutes_Me_RequiresAuth verifies /api/me returns 401 without a token,
// and returns the user when authenticated.
func TestRoutes_Me_RequiresAuth(t *testing.T) {
	lookup := &fakeLookup{
		users: map[string]*auth.User{
			"valid-token": {ID: 42, Name: "alice"},
		},
	}
	srv := httptest.NewServer(New(Deps{Lookup: lookup}))
	t.Cleanup(srv.Close)

	t.Run("no header → 401", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/me")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got == "" {
			t.Error("missing WWW-Authenticate header on 401")
		}
	})

	t.Run("wrong token → 401", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
		req.Header.Set("Authorization", "Bearer not-a-real-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("valid token → 200 with user", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body["name"] != "alice" {
			t.Errorf("body.name = %v, want alice", body["name"])
		}
		if body["id"].(float64) != 42 {
			t.Errorf("body.id = %v, want 42", body["id"])
		}
	})
}
