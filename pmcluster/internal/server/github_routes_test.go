package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// ghSecrets is the broker's per-repo secret source in these tests.
type ghSecrets struct{ secret []byte }

func (g ghSecrets) BrokerSecret(_ context.Context, repo string) ([]byte, error) {
	if repo != "acme/app" {
		return nil, store.ErrGitHubBrokerSecretNotFound
	}
	return g.secret, nil
}

// ghMinter answers without a GitHub call.
type ghMinter struct{}

func (ghMinter) Token(context.Context) (githubapp.Token, error) {
	return githubapp.Token{Token: "ghs_server_test", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// The token broker must be reachable through the real router WITHOUT a bearer
// token (a workflow has none) while every other /api route still refuses one.
func TestRoutes_GitHubTokenBrokerIsOutsideBearer(t *testing.T) {
	secret := []byte("repo-secret")
	srv := httptest.NewServer(New(Deps{
		Lookup:       &fakeLookup{},
		GitHubBroker: &githubapp.Broker{Secrets: ghSecrets{secret: secret}, Minter: ghMinter{}, RatePerMinute: -1},
	}))
	t.Cleanup(srv.Close)

	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(ts))
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/github/token", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Pmcluster-Repo", "acme/app")
	req.Header.Set("X-Pmcluster-Timestamp", ts)
	req.Header.Set("X-Pmcluster-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the broker must not sit behind Bearer)", resp.StatusCode)
	}
	var body struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Token != "ghs_server_test" || body.ExpiresAt == "" {
		t.Fatalf("body = %+v", body)
	}

	// The same router still refuses an unauthenticated /api/me.
	me, err := srv.Client().Get(srv.URL + "/api/me")
	if err != nil {
		t.Fatalf("GET /api/me: %v", err)
	}
	_ = me.Body.Close()
	if me.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/me status = %d, want 401", me.StatusCode)
	}
}
