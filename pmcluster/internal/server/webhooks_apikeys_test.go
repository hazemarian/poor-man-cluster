package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// newKeysServer wires a full store + cipher so the webhook/api-key services
// mount, with a fake Bearer lookup (token "tok" → admin, id 1). An "admin"
// user row is seeded so newly created keys get id > 1 (mirroring production
// where admin is the first user and the self-delete guard keys off id 1).
func newKeysServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ciph, err := credentials.Open(filepath.Join(dir, ".enc_key"))
	if err != nil {
		t.Fatalf("credentials.Open: %v", err)
	}
	seedUser(t, st, "admin")
	srv := httptest.NewServer(New(Deps{
		Lookup: &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
		Store:  st,
		Cipher: ciph,
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedUser creates a user row with a fresh v2 token.
func seedUser(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	tok, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	tid, secret := auth.SplitToken(tok)
	h, err := auth.HashToken(secret)
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}
	id, err := st.CreateUser(context.Background(), name, tid, h)
	if err != nil {
		t.Fatalf("CreateUser %q: %v", name, err)
	}
	return id
}

func decodeBody[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestWebhookAPI(t *testing.T) {
	srv := newKeysServer(t)
	base := srv.URL + "/api/webhooks"

	t.Run("unauth → 401", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("empty list", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		list := decodeBody[struct {
			Webhooks []struct {
				Source    string `json:"source"`
				Secret    string `json:"secret"`
				CreatedAt int64  `json:"created_at"`
			} `json:"webhooks"`
		}](t, resp)
		if len(list.Webhooks) != 0 {
			t.Fatalf("webhooks = %d, want 0", len(list.Webhooks))
		}
	})

	t.Run("create empty source → 400", func(t *testing.T) {
		resp := doJSON(t, http.MethodPost, base, "tok", map[string]string{"source": ""})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	var secret string
	t.Run("create whisper → 201, secret once", func(t *testing.T) {
		resp := doJSON(t, http.MethodPost, base, "tok", map[string]string{"source": "github", "description": "bookfair ci"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		created := decodeBody[struct {
			Source string `json:"source"`
			Secret string `json:"secret"`
		}](t, resp)
		if created.Source != "github" {
			t.Errorf("source = %q, want github", created.Source)
		}
		if len(created.Secret) != 64 {
			t.Errorf("secret len = %d, want 64 (32-byte hex)", len(created.Secret))
		}
		secret = created.Secret
	})

	t.Run("create duplicate → 409", func(t *testing.T) {
		resp := doJSON(t, http.MethodPost, base, "tok", map[string]string{"source": "github"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})

	t.Run("list shows source, never secret", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		defer resp.Body.Close()
		body := make([]byte, resp.ContentLength+1)
		n, _ := resp.Body.Read(body)
		s := string(body[:n])
		if !strings.Contains(s, `"source":"github"`) {
			t.Errorf("list missing source github: %s", s)
		}
		if strings.Contains(s, secret) {
			t.Error("list leaked the shared secret")
		}
	})

	t.Run("delete → 204, then 404", func(t *testing.T) {
		resp := doJSON(t, http.MethodDelete, base+"/github", "tok", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete status = %d, want 204", resp.StatusCode)
		}
		resp2 := doJSON(t, http.MethodDelete, base+"/github", "tok", nil)
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusNotFound {
			t.Fatalf("second delete status = %d, want 404", resp2.StatusCode)
		}
	})
}

func TestAPIKeyAPI(t *testing.T) {
	srv := newKeysServer(t)
	base := srv.URL + "/api/api_keys"

	t.Run("unauth → 401", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("list starts with seeded admin only", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		defer resp.Body.Close()
		list := decodeBody[struct {
			Keys []struct {
				Name  string `json:"name"`
				Token string `json:"token"`
			} `json:"keys"`
		}](t, resp)
		if len(list.Keys) != 1 || list.Keys[0].Name != "admin" {
			t.Fatalf("keys = %+v, want single seeded admin", list.Keys)
		}
	})

	t.Run("create empty name → 400", func(t *testing.T) {
		resp := doJSON(t, http.MethodPost, base, "tok", map[string]string{"name": ""})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	var token string
	t.Run("create ci → 201, token once", func(t *testing.T) {
		resp := doJSON(t, http.MethodPost, base, "tok", map[string]string{"name": "ci"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		created := decodeBody[struct {
			Name  string `json:"name"`
			Token string `json:"token"`
		}](t, resp)
		if created.Name != "ci" {
			t.Errorf("name = %q, want ci", created.Name)
		}
		if !strings.HasPrefix(created.Token, "pmc_") {
			t.Errorf("token should be v2 pmc_-prefixed, got %q", created.Token)
		}
		token = created.Token
	})

	t.Run("create duplicate → 409", func(t *testing.T) {
		resp := doJSON(t, http.MethodPost, base, "tok", map[string]string{"name": "ci"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})

	t.Run("list shows name, never token", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		s := string(raw)
		if !strings.Contains(s, `"name":"ci"`) {
			t.Errorf("list missing user ci: %s", s)
		}
		if strings.Contains(s, token) {
			t.Error("list leaked the bearer token")
		}
	})

	var createdID int64
	t.Run("delete by id → 204, then 404", func(t *testing.T) {
		// Find the created user's id from the list.
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		list := decodeBody[struct {
			Keys []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"keys"`
		}](t, resp)
		for _, k := range list.Keys {
			if k.Name == "ci" {
				createdID = k.ID
			}
		}
		if createdID == 0 {
			t.Fatal("created user ci not found in list")
		}

		del := doJSON(t, http.MethodDelete, base+"/"+strconv.FormatInt(createdID, 10), "tok", nil)
		del.Body.Close()
		if del.StatusCode != http.StatusNoContent {
			t.Fatalf("delete status = %d, want 204", del.StatusCode)
		}

		again := doJSON(t, http.MethodDelete, base+"/"+strconv.FormatInt(createdID, 10), "tok", nil)
		defer again.Body.Close()
		if again.StatusCode != http.StatusNotFound {
			t.Fatalf("second delete status = %d, want 404", again.StatusCode)
		}
	})

	t.Run("delete invalid id → 400", func(t *testing.T) {
		resp := doJSON(t, http.MethodDelete, base+"/abc", "tok", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("delete invalid id status = %d, want 400", resp.StatusCode)
		}
	})
}

// TestAPIKeyDeleteGuards verifies the two refusal paths on DELETE
// /api/api_keys/{id}: the "edge" console user and the currently-authenticated
// user must survive deletion attempts.
func TestAPIKeyDeleteGuards(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ciph, err := credentials.Open(filepath.Join(dir, ".enc_key"))
	if err != nil {
		t.Fatalf("credentials.Open: %v", err)
	}

	// "tok" authenticates as admin (id 1) — the self-delete target.
	// Seed admin first so "edge" gets id 2 (distinct from self-delete id 1).
	seedUser(t, st, "admin")
	seedUser(t, st, "edge")

	srv := httptest.NewServer(New(Deps{
		Lookup: &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
		Store:  st,
		Cipher: ciph,
	}))
	t.Cleanup(srv.Close)
	base := srv.URL + "/api/api_keys"

	list := decodeBody[struct {
		Keys []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"keys"`
	}](t, doJSON(t, http.MethodGet, base, "tok", nil))
	var edgeID int64
	for _, k := range list.Keys {
		if k.Name == "edge" {
			edgeID = k.ID
		}
	}
	if edgeID == 0 {
		t.Fatal("edge user not found in list")
	}

	t.Run("delete edge user → 409", func(t *testing.T) {
		resp := doJSON(t, http.MethodDelete, base+"/"+strconv.FormatInt(edgeID, 10), "tok", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("delete edge status = %d, want 409", resp.StatusCode)
		}
	})

	t.Run("delete self (admin id 1) → 409", func(t *testing.T) {
		resp := doJSON(t, http.MethodDelete, base+"/1", "tok", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("delete self status = %d, want 409", resp.StatusCode)
		}
	})
}
