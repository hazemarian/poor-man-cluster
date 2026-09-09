package openobserve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fake returns a server that records requests and dispatches to `handle`.
func fake(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(handle))
}

func jsonReply(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode reply: %v", err)
	}
}

func TestEnsureUser_CreatesWhenMissing(t *testing.T) {
	var posts int
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			jsonReply(t, w, map[string]any{"data": []map[string]any{}})
			return
		}
		posts++
		w.WriteHeader(http.StatusOK)
		jsonReply(t, w, map[string]any{"code": 200, "message": "User saved successfully"})
	})
	defer srv.Close()

	c := NewClient(srv.URL, "default")
	err := c.EnsureUser(context.Background(), "root@x.com", "rootpw", User{
		Email:    "automation@x.com",
		Password: "passw0rd!",
		Role:     "admin",
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if posts != 1 {
		t.Errorf("expected 1 POST, got %d", posts)
	}
}

func TestEnsureUser_SkipsExisting(t *testing.T) {
	var posts int
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		jsonReply(t, w, map[string]any{"data": []map[string]any{
			{"email": "Automation@x.com"},
		}})
	})
	defer srv.Close()

	c := NewClient(srv.URL, "default")
	if err := c.EnsureUser(context.Background(), "root@x.com", "rootpw", User{Email: "automation@x.com"}); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if posts != 0 {
		t.Errorf("expected no POST when user exists, got %d", posts)
	}
}

func TestEnsureIngestionToken_CreatesWhenMissing(t *testing.T) {
	var created string
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			jsonReply(t, w, map[string]any{"data": []map[string]any{}})
		default:
			body, _ := io.ReadAll(r.Body)
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(body, &p)
			created = p.Name
			jsonReply(t, w, map[string]any{"data": map[string]any{
				"name": p.Name, "token": "o2oi_newtoken12345",
			}})
		}
	})
	defer srv.Close()

	c := NewClient(srv.URL, "default")
	tok, err := c.EnsureIngestionToken(context.Background(), "root@x.com", "rootpw", "pmcluster")
	if err != nil {
		t.Fatalf("EnsureIngestionToken: %v", err)
	}
	if tok != "o2oi_newtoken12345" {
		t.Errorf("token = %q, want o2oi_newtoken12345", tok)
	}
	if created != "pmcluster" {
		t.Errorf("created name = %q, want pmcluster", created)
	}
}

func TestEnsureIngestionToken_ReusesExisting(t *testing.T) {
	var posts int
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		jsonReply(t, w, map[string]any{"data": []map[string]any{
			{"name": "pmcluster", "token": "o2oi_existingtoken"},
		}})
	})
	defer srv.Close()

	c := NewClient(srv.URL, "default")
	tok, err := c.EnsureIngestionToken(context.Background(), "root@x.com", "rootpw", "pmcluster")
	if err != nil {
		t.Fatalf("EnsureIngestionToken: %v", err)
	}
	if tok != "o2oi_existingtoken" {
		t.Errorf("token = %q, want o2oi_existingtoken", tok)
	}
	if posts != 0 {
		t.Errorf("expected no POST when token exists, got %d", posts)
	}
}

func TestClient_Non2xxIsError(t *testing.T) {
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	defer srv.Close()

	c := NewClient(srv.URL, "default")
	err := c.EnsureUser(context.Background(), "root@x.com", "rootpw", User{Email: "a@b.c"})
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code, got: %v", err)
	}
}
