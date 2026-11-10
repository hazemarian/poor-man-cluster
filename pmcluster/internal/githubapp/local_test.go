package githubapp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func newLocal(t *testing.T) (*Local, *fakeGitHub, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cipher, err := credentials.Open(filepath.Join(dir, ".encryption_key"))
	if err != nil {
		t.Fatalf("open cipher: %v", err)
	}
	fake := newFakeGitHub(t)
	l := &Local{Store: st, Cipher: cipher, Client: fake.client()}
	return l, fake, dbPath
}

func TestLocal_SaveStatusAndClear(t *testing.T) {
	l, _, _ := newLocal(t)
	ctx := context.Background()

	if l.Configured(ctx) {
		t.Fatal("a fresh store must not report a configured app")
	}
	if _, err := l.Minter(ctx); !errors.Is(err, ErrNoApp) {
		t.Fatalf("Minter on an empty store = %v, want ErrNoApp", err)
	}

	key := testKeyPEM(t)
	if err := l.Save(ctx, 4242, 7, key, "acme/app", "acme/app:latest"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := l.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Configured || st.AppID != 4242 || st.InstallationID != 7 {
		t.Fatalf("status = %+v", st)
	}
	if st.Warning != "" {
		t.Errorf("warning = %q, want none (both permissions are granted)", st.Warning)
	}
	if st.Permissions["packages"] != "read" {
		t.Errorf("permissions = %v", st.Permissions)
	}

	// Saving again without a key keeps the stored key (rotation of ids only).
	if err := l.Save(ctx, 4242, 8, nil, "acme/app", "acme/app:latest"); err != nil {
		t.Fatalf("Save without a key: %v", err)
	}
	st, _ = l.Status(ctx)
	if st.InstallationID != 8 {
		t.Errorf("installation id = %d, want 8", st.InstallationID)
	}

	if err := l.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if l.Configured(ctx) {
		t.Error("Clear left the app configured")
	}
	// Clearing an unconfigured app is idempotent.
	if err := l.Clear(ctx); err != nil {
		t.Errorf("second Clear: %v", err)
	}
}

func TestLocal_SaveRejectsABadKey(t *testing.T) {
	l, _, _ := newLocal(t)
	err := l.Save(context.Background(), 1, 2, []byte("not a key"), "", "")
	if !errors.Is(err, ErrBadKey) {
		t.Fatalf("Save = %v, want ErrBadKey", err)
	}
}

func TestLocal_DiagnoseProbesRepoAndPackage(t *testing.T) {
	l, _, _ := newLocal(t)
	ctx := context.Background()
	if err := l.Save(ctx, 4242, 7, testKeyPEM(t), "acme/app", "acme/app:latest"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var repoHits, pkgHits atomic.Int32
	// Mint against the API fake first so the cache holds a token; the repo probe
	// then runs against the probe server below (a different base URL).
	if _, err := l.Token(ctx); err != nil {
		t.Fatalf("prime token: %v", err)
	}
	// The API fake handles /repos; the registry fake proves the basic-auth
	// shape docker login uses.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/contents/", func(w http.ResponseWriter, r *http.Request) {
		repoHits.Add(1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	l.Client.APIBase = api.URL

	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pkgHits.Add(1)
		user, pass, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || !strings.HasPrefix(pass, "ghs_") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(reg.Close)
	l.RegistryBase = reg.URL

	detail, expires, err := l.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if repoHits.Load() != 1 || pkgHits.Load() != 1 {
		t.Errorf("probes = repo %d / package %d, want 1/1", repoHits.Load(), pkgHits.Load())
	}
	if expires.IsZero() {
		t.Error("Diagnose did not report the token expiry")
	}
	if !strings.Contains(detail, "acme/app") {
		t.Errorf("detail = %q, want it to name the probe targets", detail)
	}
}

func TestLocal_DiagnoseWithoutProbeTarget(t *testing.T) {
	l, _, _ := newLocal(t)
	ctx := context.Background()
	if err := l.Save(ctx, 4242, 7, testKeyPEM(t), "", ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, _, err := l.Diagnose(ctx)
	if !errors.Is(err, ErrNoProbeTarget) {
		t.Fatalf("Diagnose = %v, want ErrNoProbeTarget", err)
	}
	res := l.Test(ctx)
	if !res.OK {
		t.Errorf("Test without a probe target = %+v, want OK with a hint", res)
	}
}

func TestLocal_MissingPermissionIsNamedInStatus(t *testing.T) {
	l, fake, _ := newLocal(t)
	fake.permissions = map[string]string{"contents": "read"}
	if err := l.Save(context.Background(), 4242, 7, testKeyPEM(t), "", ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := l.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(st.Warning, PermPackagesRead) {
		t.Errorf("warning = %q, want it to name %s", st.Warning, PermPackagesRead)
	}
}

func TestLocal_BrokerSecretRoundTrip(t *testing.T) {
	l, _, _ := newLocal(t)
	ctx := context.Background()

	secret, err := l.CreateBrokerSecret(ctx, "acme/app", "deploy workflow")
	if err != nil {
		t.Fatalf("CreateBrokerSecret: %v", err)
	}
	if len(secret) != 64 {
		t.Errorf("secret length = %d, want 64 hex chars", len(secret))
	}
	got, err := l.BrokerSecret(ctx, "acme/app")
	if err != nil {
		t.Fatalf("BrokerSecret: %v", err)
	}
	if string(got) != secret {
		t.Errorf("stored secret does not round-trip")
	}
	if _, err := l.CreateBrokerSecret(ctx, "acme/app", ""); !errors.Is(err, store.ErrGitHubBrokerSecretExists) {
		t.Errorf("duplicate create = %v, want ErrGitHubBrokerSecretExists", err)
	}
	list, err := l.ListBrokerSecrets(ctx)
	if err != nil || len(list) != 1 || list[0].Repo != "acme/app" {
		t.Fatalf("ListBrokerSecrets = %+v, %v", list, err)
	}
	if err := l.MarkBrokerSecretUsed(ctx, "acme/app"); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if err := l.DeleteBrokerSecret(ctx, "acme/app"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := l.BrokerSecret(ctx, "acme/app"); !errors.Is(err, store.ErrGitHubBrokerSecretNotFound) {
		t.Errorf("after delete = %v, want not found", err)
	}
	if _, err := l.CreateBrokerSecret(ctx, "not-a-repo", ""); err == nil {
		t.Error("CreateBrokerSecret accepted a name that is not owner/name")
	}
}

// TestLocal_TokenIsNeverPersisted is the storage half of the credential rule:
// the app key is ciphertext at rest, and the derived token never reaches the
// database file at all.
func TestLocal_TokenIsNeverPersisted(t *testing.T) {
	l, fake, dbPath := newLocal(t)
	ctx := context.Background()
	if err := l.Save(ctx, 4242, 7, testKeyPEM(t), "", ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	tok, err := l.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.Token != fake.token {
		t.Fatalf("token = %q", tok.Token)
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	if bytes.Contains(raw, []byte(fake.token)) {
		t.Error("the installation token was written to data.db")
	}
	if bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Error("the private key was stored in plaintext")
	}
}

// TestProbePackage_WrongTagNamesTheRealTags: GHCR answers 401 for a tag that
// does not exist, exactly as it does for a token without access, so the probe
// used to report "the token was refused" for what is almost always a typo in
// the reference. A 200 on the tag list proves the package exists, which turns
// that ambiguity into "it has no tag X — its tags are …".
func TestProbePackage_WrongTagNamesTheRealTags(t *testing.T) {
	l, _, _ := newLocal(t)
	ctx := context.Background()

	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifests/latest"):
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"acme/app","tags":["v1","v20261109-abc1234","v0","v2","v3","v4"]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(reg.Close)
	l.RegistryBase = reg.URL

	err := l.probePackage(ctx, "ghs_test", "acme/app:latest")
	if !errors.Is(err, ErrNoProbeTarget) {
		t.Fatalf("err = %v, want ErrNoProbeTarget (a wrong tag is not an auth failure)", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `no tag "latest"`) {
		t.Errorf("message = %q, want it to name the missing tag", msg)
	}
	for _, want := range []string{"v20261109-abc1234", "v4"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want it to list the real tag %s", msg, want)
		}
	}
	if strings.Contains(msg, "v0") {
		t.Errorf("message = %q, want at most five tags so the error stays readable", msg)
	}
}

// TestProbePackage_401WithoutTagsKeepsTheAmbiguityHonest: when the tag list is
// refused too, the probe must not claim to know which of the two causes it is.
func TestProbePackage_401WithoutTagsKeepsTheAmbiguityHonest(t *testing.T) {
	l, _, _ := newLocal(t)
	ctx := context.Background()

	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(reg.Close)
	l.RegistryBase = reg.URL

	err := l.probePackage(ctx, "ghs_test", "acme/nope:v1")
	if !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("err = %v, want ErrTokenRejected", err)
	}
	msg := err.Error()
	for _, want := range []string{"401", "does not distinguish", "packages:read"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want it to contain %q", msg, want)
		}
	}
}
