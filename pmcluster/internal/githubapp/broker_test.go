package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

type mapSecrets map[string][]byte

func (m mapSecrets) BrokerSecret(_ context.Context, repo string) ([]byte, error) {
	sec, ok := m[repo]
	if !ok {
		return nil, store.ErrGitHubBrokerSecretNotFound
	}
	return sec, nil
}

func (m mapSecrets) MarkBrokerSecretUsed(_ context.Context, repo string) error {
	m["used:"+repo] = []byte("1")
	return nil
}

type stubMinter struct {
	tok   Token
	err   error
	calls int
}

func (s *stubMinter) Token(context.Context) (Token, error) {
	s.calls++
	return s.tok, s.err
}

func newBrokerServer(t *testing.T, b *Broker) *httptest.Server {
	t.Helper()
	// The Bearer-protected /api subtree is registered first on purpose: the
	// broker must win the exact path even though a subrouter owns /api/*.
	r := chi.NewRouter()
	r.Route("/api", func(sr chi.Router) {
		sr.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "bearer required", http.StatusUnauthorized)
			})
		})
		sr.Get("/me", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	})
	b.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestBroker_MintsForASignedRepo(t *testing.T) {
	secret := []byte("repo-secret")
	minter := &stubMinter{tok: Token{Token: "ghs_minted", ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}}
	b := &Broker{
		Secrets:       mapSecrets{"acme/app": secret},
		Minter:        minter,
		RatePerMinute: -1, // limiting is exercised separately
		Now:           func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	srv := newBrokerServer(t, b)

	req := signedRequest(t, srv.URL+"/api/github/token", "acme/app", secret, "", time.Unix(1_800_000_000, 0))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Token != "ghs_minted" {
		t.Errorf("token = %q", got.Token)
	}
	if _, err := time.Parse(time.RFC3339, got.ExpiresAt); err != nil {
		t.Errorf("expires_at %q is not RFC3339: %v", got.ExpiresAt, err)
	}
	if minter.calls != 1 {
		t.Errorf("minter called %d times, want 1", minter.calls)
	}
	if b.Secrets.(mapSecrets)["used:acme/app"] == nil {
		t.Error("a successful mint should record last use")
	}
}

// TestBroker_MountOrderDoesNotConflict registers the broker BEFORE the
// Bearer-protected /api subtree, which is what internal/server.New does. chi
// must still match the exact path rather than swallowing it in the subtree.
func TestBroker_MountOrderDoesNotConflict(t *testing.T) {
	secret := []byte("repo-secret")
	now := time.Unix(1_800_000_000, 0)
	b := &Broker{
		Secrets:       mapSecrets{"acme/app": secret},
		Minter:        &stubMinter{tok: Token{Token: "ghs_minted", ExpiresAt: now.Add(time.Hour)}},
		RatePerMinute: -1,
		Now:           func() time.Time { return now },
	}
	r := chi.NewRouter()
	b.Mount(r)
	r.Route("/api", func(sr chi.Router) {
		sr.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "bearer required", http.StatusUnauthorized)
			})
		})
		sr.Get("/me", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	req := signedRequest(t, srv.URL+"/api/github/token", "acme/app", secret, "", now)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the broker must win its exact path)", resp.StatusCode)
	}
}

func TestBroker_RejectsUnauthenticatedCallers(t *testing.T) {
	secret := []byte("repo-secret")
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"no headers", func(r *http.Request) {
			r.Header.Del(BrokerRepoHeader)
			r.Header.Del(BrokerTimestampHeader)
			r.Header.Del(BrokerSignatureHeader)
		}},
		{"bad signature", func(r *http.Request) {
			r.Header.Set(BrokerSignatureHeader, "sha256="+strings.Repeat("0", 64))
		}},
		{"stale timestamp", func(r *http.Request) {
			ts := now.Add(-10 * time.Minute).Unix()
			r.Header.Set(BrokerTimestampHeader, itoa(ts))
			r.Header.Set(BrokerSignatureHeader, "sha256="+hmacHex(secret, itoa(ts)))
		}},
		{"unknown repo", func(r *http.Request) {
			r.Header.Set(BrokerRepoHeader, "acme/other")
			r.Header.Set(BrokerSignatureHeader, "sha256="+hmacHex(secret, itoa(now.Unix())))
		}},
		{"signature without scheme", func(r *http.Request) {
			r.Header.Set(BrokerSignatureHeader, strings.Repeat("a", 64))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			minter := &stubMinter{tok: Token{Token: "ghs_minted"}}
			b := &Broker{Secrets: mapSecrets{"acme/app": secret}, Minter: minter,
				RatePerMinute: -1, Now: func() time.Time { return now }}
			srv := newBrokerServer(t, b)
			req := signedRequest(t, srv.URL+"/api/github/token", "acme/app", secret, "", now)
			tc.mutate(req)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if minter.calls != 0 {
				t.Error("an unauthenticated request reached the minter")
			}
		})
	}
}

func TestBroker_RateLimitRefusesTheBurstAndBeyond(t *testing.T) {
	secret := []byte("repo-secret")
	now := time.Unix(1_800_000_000, 0)
	b := &Broker{
		Secrets:       mapSecrets{"acme/app": secret},
		Minter:        &stubMinter{tok: Token{Token: "ghs_minted"}},
		RatePerMinute: 60, Burst: 2,
		Now: func() time.Time { return now },
	}
	srv := newBrokerServer(t, b)

	statuses := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		req := signedRequest(t, srv.URL+"/api/github/token", "acme/app", secret, "", now)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST %d: %v", i, err)
		}
		_ = resp.Body.Close()
		statuses = append(statuses, resp.StatusCode)
	}
	if statuses[0] != http.StatusOK || statuses[1] != http.StatusOK {
		t.Fatalf("first two mints = %v, want [200 200]", statuses[:2])
	}
	if statuses[2] != http.StatusTooManyRequests {
		t.Fatalf("third mint = %d, want 429", statuses[2])
	}
}

func TestBroker_NoAppIsNotA500(t *testing.T) {
	secret := []byte("repo-secret")
	now := time.Unix(1_800_000_000, 0)
	b := &Broker{
		Secrets:       mapSecrets{"acme/app": secret},
		Minter:        &stubMinter{err: ErrNoApp},
		RatePerMinute: -1,
		Now:           func() time.Time { return now },
	}
	srv := newBrokerServer(t, b)
	req := signedRequest(t, srv.URL+"/api/github/token", "acme/app", secret, "", now)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	var got map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if !strings.Contains(got["error"], "not configured") {
		t.Errorf("error = %q, want it to name the missing app", got["error"])
	}
}

func TestBroker_MintErrorsKeepTheirCause(t *testing.T) {
	b := &Broker{}
	cases := []struct {
		err  error
		want string
	}{
		{ErrBadKey, "key was rejected"},
		{ErrInstallationNotFound, "installation is gone"},
		{ErrInsufficientPermission, "permission"},
		{errors.New("boom"), "mint installation token"},
	}
	for _, tc := range cases {
		if got := b.mintError(tc.err); !strings.Contains(got, tc.want) {
			t.Errorf("mintError(%v) = %q, want it to mention %q", tc.err, got, tc.want)
		}
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
