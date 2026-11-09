package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/doctor"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/githubapp"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// twoLevelManifest is the ordered-deploy shape: the deployer receives two
// `docker stack deploy` calls for ONE logical deploy. A registry login belongs
// to the deploy, not to each level, so the count must still be one.
const twoLevelManifest = `
app: ordered-app
env: production
domain: example.com
services:
  db:
    image: postgres:14-alpine
  api:
    image: ghcr.io/acme/app:latest
    depends_on: [db]
`

// TestDeployLogsInExactlyOnceEvenAcrossDependsOnLevels: the credential is
// applied once per deploy, before the FIRST `docker stack deploy
// --with-registry-auth` freezes it into a spec. Two levels, one login.
func TestDeployLogsInExactlyOnceEvenAcrossDependsOnLevels(t *testing.T) {
	st, cfg := seedRegistry(t, ghcrHost, "pull-bot", "ghp_readonly_token")

	var mu sync.Mutex
	var got []string
	restore := dockerLoginExec
	dockerLoginExec = func(_ context.Context, host, username string, password []byte) error {
		mu.Lock()
		got = append(got, host+"|"+username+"|"+string(password))
		mu.Unlock()
		return nil
	}
	defer func() { dockerLoginExec = restore }()

	dep := &countingDeployer{}
	var log logCapture
	svc := &stacks.Service{
		Store:    st,
		Deployer: dep,
		MkdirAll: func(string, os.FileMode) error { return nil },
		BeforeApply: func(ctx context.Context) {
			loginBeforeDeploy(ctx, st, cfg, log.logger())
		},
	}

	if _, err := svc.Deploy(context.Background(), stacks.Payload{Manifest: twoLevelManifest}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if dep.levels != 2 {
		t.Fatalf("dependency levels deployed = %d, want 2 (the test must exercise the multi-level path)", dep.levels)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("%d docker login(s) for one deploy, want exactly 1: %v", len(got), got)
	}
	if got[0] != ghcrHost+"|pull-bot|ghp_readonly_token" {
		t.Fatalf("docker login = %q, want the STORED credential for %s (not an App token)", got[0], ghcrHost)
	}
}

// TestDeployWarnsWithCredentialAgeAndRotation: the warning is the operator's
// early-warning system for the 90-day PAT expiry, so it must name the registry,
// the credential's source, its age and the rotation command.
func TestDeployWarnsWithCredentialAgeAndRotation(t *testing.T) {
	st, cfg := seedRegistry(t, ghcrHost, "pull-bot", "ghp_readonly_token")
	row, err := st.Registry(context.Background(), ghcrHost)
	if err != nil {
		t.Fatalf("read seeded registry: %v", err)
	}

	restoreNow := registryNow
	registryNow = func() time.Time { return time.Unix(row.CreatedAt, 0).Add(70 * 24 * time.Hour) }
	defer func() { registryNow = restoreNow }()

	restore := dockerLoginExec
	dockerLoginExec = func(context.Context, string, string, []byte) error { return nil }
	defer func() { dockerLoginExec = restore }()

	var log logCapture
	loginBeforeDeploy(context.Background(), st, cfg, log.logger())

	entry := log.find("docker logged in to " + ghcrHost)
	if entry == nil {
		t.Fatalf("no deploy login warning captured; log:\n%s", log.text())
	}
	if entry.Level != "warn" {
		t.Errorf("deploy login logged at %q, want warn", entry.Level)
	}
	for _, want := range []string{"stored registry credential", "70 days ago", "expire at 90 days", "rotate with", "pmcluster registry add"} {
		if !strings.Contains(entry.Message, want) {
			t.Errorf("warning does not carry %q: %q", want, entry.Message)
		}
	}
	var source string
	_ = json.Unmarshal(entry.Fields["credential_source"], &source)
	if source != "stored registry credential" {
		t.Errorf("credential_source = %q, want \"stored registry credential\"", source)
	}
}

// TestDeployLoginFailureWarnsAndDoesNotBlock: a rejected login must not refuse
// the deploy — docker's own push/pull error names the problem more precisely,
// and doctor is what reports the credential before a deploy needs it.
func TestDeployLoginFailureWarnsAndDoesNotBlock(t *testing.T) {
	st, cfg := seedRegistry(t, ghcrHost, "pull-bot", "ghp_revoked")

	restore := dockerLoginExec
	dockerLoginExec = func(context.Context, string, string, []byte) error {
		return errors.New("exit status 1: unauthorized: authentication failed")
	}
	defer func() { dockerLoginExec = restore }()

	var log logCapture
	loginBeforeDeploy(context.Background(), st, cfg, log.logger())

	entry := log.find("failed")
	if entry == nil {
		t.Fatalf("no failure warning captured; log:\n%s", log.text())
	}
	if entry.Level != "warn" {
		t.Errorf("login failure logged at %q, want warn", entry.Level)
	}
	if !strings.Contains(entry.Message+string(entry.Fields["error"]), "unauthorized") {
		t.Errorf("failure warning dropped docker's stderr: %+v", entry)
	}
}

// TestRegistryLoginsSerialise: a deploy and the startup replay both write
// docker's config.json, so the exec must never overlap. The fake widens its own
// window; the assertion is observed concurrency, not timing.
func TestRegistryLoginsSerialise(t *testing.T) {
	st, cfg := seedRegistry(t, ghcrHost, "pull-bot", "ghp_readonly_token")

	var inFlight, maxInFlight, calls atomic.Int32
	restore := dockerLoginExec
	dockerLoginExec = func(context.Context, string, string, []byte) error {
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		calls.Add(1)
		for i := 0; i < 2000; i++ {
			runtime.Gosched()
		}
		inFlight.Add(-1)
		return nil
	}
	defer func() { dockerLoginExec = restore }()

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			loginBeforeDeploy(context.Background(), st, cfg, zerolog.Nop())
		}()
	}
	close(start)
	wg.Wait()

	if got := calls.Load(); got != n {
		t.Fatalf("%d of %d logins reached docker", got, n)
	}
	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("%d logins ran concurrently; every docker login must hold the same mutex", got)
	}
}

// TestDockerLoginEnvPinsDockerConfig: the unit's environment may carry another
// DOCKER_CONFIG (or none at all), so the login pins the directory a deploy
// reads rather than inheriting one.
func TestDockerLoginEnvPinsDockerConfig(t *testing.T) {
	env := dockerLoginEnv([]string{"HOME=/root", "DOCKER_CONFIG=/tmp/elsewhere", "PATH=/usr/bin"})
	var seen []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "DOCKER_CONFIG=") {
			seen = append(seen, kv)
		}
	}
	if len(seen) != 1 || seen[0] != "DOCKER_CONFIG=/root/.docker" {
		t.Fatalf("DOCKER_CONFIG entries = %v, want exactly [DOCKER_CONFIG=/root/.docker]", seen)
	}
	if len(env) != 3 {
		t.Fatalf("env has %d entries, want 3 (two preserved + the pinned one)", len(env))
	}
}

// TestNoLoginTickerOrIntervalRemains is the inverse of the guard that once
// pinned the 25-minute refresh: the App-token pull login, its ticker and its
// leadership-gain mint must not come back. The pull credential is a long-lived
// PAT applied at deploy; nothing refreshes it on an interval.
func TestNoLoginTickerOrIntervalRemains(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))

	forbidden := []string{
		"refresh" + "GitHubAppLogins",
		"loginOn" + "LeadershipGain",
		"leader" + "GainSteps",
		"renewed every 25 " + "minutes",
		"25*time." + "Minute",
	}

	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || path == file || filepath.Ext(path) != ".go" {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			for _, token := range forbidden {
				if strings.Contains(string(body), token) {
					offenders = append(offenders, rel+" contains "+token)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("the App-token pull login is still in the tree:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestDoctorGitHubAppCheckMintsWithoutAnExternalHelper: the doctor check runs
// the daemon's own client against GitHub with an empty PATH — no helper binary,
// no other process.
func TestDoctorGitHubAppCheckMintsWithoutAnExternalHelper(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	st, cfg, srv := seedCLIApp(t, "acme/app")
	defer srv.Close()

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		t.Fatalf("open cipher: %v", err)
	}
	probe := newGitHubAppLocal(st, cipher, zerolog.Nop())

	rep, err := doctor.Run(context.Background(), doctor.Input{GitHubApp: probe})
	if err != nil {
		t.Fatalf("doctor.Run: %v", err)
	}
	var got *doctor.Check
	for i := range rep.Checks {
		if rep.Checks[i].Name == "github app" {
			got = &rep.Checks[i]
		}
	}
	if got == nil {
		t.Fatal("doctor.Run reported no `github app` check")
	}
	if got.Status != doctor.StatusOK {
		t.Fatalf("github app = %s (%s), want ok: the check must mint and probe without a helper", got.Status, got.Detail)
	}
}

// TestNoSourceFileMentionsTheRemovedCredentialHelper: the pull-time helper is
// what must not come back. Anything still naming it — a route, a port, a config
// key, a doc paragraph — describes a mechanism that no longer ships.
func TestNoSourceFileMentionsTheRemovedCredentialHelper(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))

	// Split so this file does not match itself; it is skipped anyway.
	forbidden := []string{
		"pull" + "cred",
		"docker-credential-" + "pmcluster",
		"/cred/v1/" + "token",
		"cred" + "Helpers",
		"9443",
		"EnsureDocker" + "CredentialHelper",
		"Pull" + "CredentialPath",
	}

	texts := map[string]bool{
		".go": true, ".md": true, ".yaml": true, ".yml": true,
		".sh": true, ".tmpl": true, ".service": true, ".json": true,
	}

	// CHANGELOG.md is a history file: naming the machinery a revert removed is
	// its job, not a stale instruction. docs/test-reports hold past reports.
	skip := map[string]bool{
		filepath.Join(root, "CHANGELOG.md"):         true,
		filepath.Join(root, "docs", "test-reports"): true,
	}

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata":
				return filepath.SkipDir
			}
			if skip[path] {
				return filepath.SkipDir
			}
			return nil
		}
		if path == file || skip[path] || !texts[filepath.Ext(path)] {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, token := range forbidden {
			if strings.Contains(string(body), token) {
				offenders = append(offenders, rel+" contains "+token)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(offenders) > 0 {
		t.Fatalf("removed pull-time credential helper is still referenced:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// countingDeployer is a minimal runtime.StackDeployer: it records how many
// per-level deploys reached docker, which is what proves the login did NOT
// happen once per level.
type countingDeployer struct {
	mu     sync.Mutex
	levels int
}

func (d *countingDeployer) DeployStack(context.Context, string, []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.levels++
	return nil
}

func (d *countingDeployer) DeployStackNoPrune(context.Context, string, []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.levels++
	return nil
}

func (d *countingDeployer) PruneStack(context.Context, string, []byte) error { return nil }
func (d *countingDeployer) RemoveStack(context.Context, string) error        { return nil }
func (d *countingDeployer) ForceUpdateService(context.Context, string) error { return nil }
func (d *countingDeployer) PruneStaleContainers(context.Context, string, string) error {
	return nil
}

// seedRegistry stores one encrypted registry credential.
func seedRegistry(t *testing.T, host, username, password string) (*store.Store, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{DataDir: dir}

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		t.Fatalf("open cipher: %v", err)
	}
	ct, err := cipher.Encrypt([]byte(password))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := st.CreateRegistry(context.Background(), &store.Registry{
		Host: host, Username: username, PasswordCiphertext: ct,
	}); err != nil {
		t.Fatalf("create registry: %v", err)
	}
	return st, cfg
}

// seedCLIApp stores an app in a temp store and points the CLI's GitHub client
// at a fake API that mints one token and answers the repo probe.
func seedCLIApp(t *testing.T, probeRepo string) (*store.Store, *config.Config, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{DataDir: dir}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"ghs_refresh_test","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `[]`)
	}))

	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		srv.Close()
		t.Fatalf("open cipher: %v", err)
	}
	local := &githubapp.Local{Store: st, Cipher: cipher, Client: &githubapp.Client{APIBase: srv.URL, HTTP: srv.Client()}}
	if err := local.Save(context.Background(), 4242, 7, refreshTestKeyPEM(t), probeRepo, ""); err != nil {
		srv.Close()
		t.Fatalf("save app: %v", err)
	}

	restore := newGitHubClient
	newGitHubClient = func(*zerolog.Logger) *githubapp.Client {
		return &githubapp.Client{APIBase: srv.URL, HTTP: srv.Client()}
	}
	t.Cleanup(func() { newGitHubClient = restore })

	return st, cfg, srv
}

func refreshTestKeyPEM(t *testing.T) []byte {
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

// logCapture is a zerolog sink a test can read back, so the warning is
// asserted as emitted output rather than as a string in the source.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) logger() zerolog.Logger {
	return zerolog.New(&lockedWriter{c: c})
}

func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type logEntry struct {
	Level   string
	Message string
	Fields  map[string]json.RawMessage
}

// find returns the first entry whose message contains substr.
func (c *logCapture) find(substr string) *logEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		var msg string
		_ = json.Unmarshal(raw["message"], &msg)
		if !strings.Contains(msg, substr) {
			continue
		}
		var lvl string
		_ = json.Unmarshal(raw["level"], &lvl)
		return &logEntry{Level: lvl, Message: msg, Fields: raw}
	}
	return nil
}

func (c *logCapture) contains(substr string) bool { return c.find(substr) != nil }

type lockedWriter struct{ c *logCapture }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	return w.c.buf.Write(p)
}
