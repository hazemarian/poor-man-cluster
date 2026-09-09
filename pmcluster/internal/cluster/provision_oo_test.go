package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/openobserve"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// fakeOOServer mimics the OpenObserve ingestion-token + users API used by the
// provisioner. It tracks created users/tokens and is concurrency-safe.
type fakeOOServer struct {
	mu        sync.Mutex
	users     []map[string]string // email->role
	tokens    []map[string]string // name->token
	userPosts int
	tokPosts  int
}

func (f *fakeOOServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/api/default/users" {
			if r.Method == http.MethodGet {
				data := make([]map[string]any, 0, len(f.users))
				for _, u := range f.users {
					data = append(data, map[string]any{"email": u["email"]})
				}
				writeJSON(w, map[string]any{"data": data})
				return
			}
			if r.Method == http.MethodPost {
				f.userPosts++
				var p struct {
					Email    string `json:"email"`
					Password string `json:"password"`
					Role     string `json:"role"`
				}
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &p)
				f.users = append(f.users, map[string]string{"email": p.Email, "password": p.Password, "role": p.Role})
				writeJSON(w, map[string]any{"code": 200, "message": "User saved successfully"})
				return
			}
		}
		if r.URL.Path == "/api/default/ingestion-tokens" {
			if r.Method == http.MethodGet {
				data := make([]map[string]any, 0, len(f.tokens))
				for _, t := range f.tokens {
					data = append(data, map[string]any{"name": t["name"], "token": t["token"]})
				}
				writeJSON(w, map[string]any{"data": data})
				return
			}
			if r.Method == http.MethodPost {
				f.tokPosts++
				var p struct {
					Name string `json:"name"`
				}
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &p)
				tok := "o2oi_" + p.Name + "_tok3n"
				f.tokens = append(f.tokens, map[string]string{"name": p.Name, "token": tok})
				writeJSON(w, map[string]any{"data": map[string]any{"name": p.Name, "token": tok}})
				return
			}
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestProvision_EnsureUserAndToken_IsIdempotent(t *testing.T) {
	fake := &fakeOOServer{tokens: []map[string]string{{"name": "other", "token": "o2oi_other"}}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cipher := openTestCipher(t, filepath.Join(dir, ".key"))
	rootPass, _ := RandomPassword()
	ct, _ := cipher.Encrypt([]byte(rootPass))
	if err := s.InsertCredential(context.Background(), &store.ManagedCredential{
		Name: "openobserve_admin", Kind: string(KindOpenObserve),
		Username: "root@example.com", PasswordCiphertext: ct,
	}); err != nil {
		t.Fatalf("insert root: %v", err)
	}

	p := &OpenObserveProvisioner{
		Store:  s,
		Cipher: cipher,
		Client: openobserve.NewClient(srv.URL, "default"),
	}

	// First run creates user + token.
	user1, tok1, err := p.EnsureUserAndToken(context.Background())
	if err != nil {
		t.Fatalf("first EnsureUserAndToken: %v", err)
	}
	if user1 == nil {
		t.Fatal("expected user credential on first run")
	}
	if tok1 == nil {
		t.Fatal("expected token credential on first run")
	}
	if fake.userPosts != 1 || fake.tokPosts != 1 {
		t.Errorf("first run: userPosts=%d tokPosts=%d, want 1/1", fake.userPosts, fake.tokPosts)
	}

	// Second run must reuse both — no new API calls, same stored values.
	user2, tok2, err := p.EnsureUserAndToken(context.Background())
	if err != nil {
		t.Fatalf("second EnsureUserAndToken: %v", err)
	}
	if user2 == nil {
		t.Fatal("expected user credential on second run")
	}
	if tok2 == nil {
		t.Fatal("expected token credential on second run")
	}
	if fake.userPosts != 1 || fake.tokPosts != 1 {
		t.Errorf("second run changed API calls: userPosts=%d tokPosts=%d, want 1/1", fake.userPosts, fake.tokPosts)
	}
	if user1 == nil || user2 == nil {
		t.Fatalf("user credentials unexpectedly nil on second run")
	}
	if user1.Username != user2.Username {
		t.Errorf("user email changed between runs: %q != %q", user1.Username, user2.Username)
	}
	if tok1 == nil || tok2 == nil {
		t.Fatalf("token credentials unexpectedly nil on second run")
	}
	if string(tok1.PasswordCiphertext) != string(tok2.PasswordCiphertext) {
		t.Error("token ciphertext changed between runs — was re-minted")
	}

	// The stored token's username should be the org ("default").
	if tok2.Username != "default" {
		t.Errorf("token credential username = %q, want default (org)", tok2.Username)
	}
}
