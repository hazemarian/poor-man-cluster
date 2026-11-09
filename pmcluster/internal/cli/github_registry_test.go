package cli

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func TestRegistryLoginTargets(t *testing.T) {
	regs := []*store.Registry{
		{Host: "ghcr.io"},
		{Host: "docker.io"},
		{Host: "registry.example.com"},
	}
	withApp := registryLoginTargets(regs, true)
	if len(withApp) != 2 {
		t.Fatalf("app applied: %d targets, want 2", len(withApp))
	}
	for _, r := range withApp {
		if r.Host == "ghcr.io" {
			t.Error("the stored ghcr.io credential must be dropped when the App token was applied")
		}
	}
	if got := registryLoginTargets(regs, false); len(got) != 3 {
		t.Fatalf("no app: %d targets, want all 3", len(got))
	}
}

func TestGitHubAppLogin_NoAppIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	cfg := &config.Config{DataDir: dir}

	called := false
	restore := dockerLoginExec
	dockerLoginExec = func(context.Context, string, string, []byte) error {
		called = true
		return nil
	}
	defer func() { dockerLoginExec = restore }()

	configured, err := githubAppLogin(context.Background(), st, cfg, zerolog.Nop())
	if err != nil {
		t.Fatalf("githubAppLogin: %v", err)
	}
	if configured {
		t.Error("configured = true without an app")
	}
	if called {
		t.Error("docker login ran without an app")
	}
}

func TestGitHubAppLogin_LogsInWithTheInstallationToken(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	cfg := &config.Config{DataDir: dir}

	// A fake GitHub that mints one token; no test reaches github.com.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"ghs_cli_test","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	}))
	defer srv.Close()

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		t.Fatalf("open cipher: %v", err)
	}
	// A PEM key is needed by the store; the fake never validates the JWT.
	key := testCLIKeyPEM(t)
	l := &githubapp.Local{Store: st, Cipher: cipher, Client: &githubapp.Client{APIBase: srv.URL}}
	if err := l.Save(context.Background(), 4242, 7, key, "", ""); err != nil {
		t.Fatalf("Save: %v", err)
	}

	restoreClient := newGitHubClient
	newGitHubClient = func(*zerolog.Logger) *githubapp.Client {
		return &githubapp.Client{APIBase: srv.URL, HTTP: srv.Client()}
	}
	defer func() { newGitHubClient = restoreClient }()

	var gotHost, gotUser, gotPass string
	restore := dockerLoginExec
	dockerLoginExec = func(_ context.Context, host, user string, password []byte) error {
		gotHost, gotUser, gotPass = host, user, string(password)
		return nil
	}
	defer func() { dockerLoginExec = restore }()

	configured, err := githubAppLogin(context.Background(), st, cfg, zerolog.Nop())
	if err != nil {
		t.Fatalf("githubAppLogin: %v", err)
	}
	if !configured {
		t.Fatal("configured = false with an app stored")
	}
	if gotHost != "ghcr.io" || gotUser != "x-access-token" || gotPass != "ghs_cli_test" {
		t.Fatalf("docker login = %s -u %s --password-stdin (password %d bytes), want ghcr.io / x-access-token / the minted token",
			gotHost, gotUser, len(gotPass))
	}
}

// testCLIKeyPEM is a throwaway RSA key in the PKCS#8 shape GitHub hands out.
func testCLIKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
