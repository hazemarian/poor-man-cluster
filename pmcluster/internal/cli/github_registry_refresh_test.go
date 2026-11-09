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
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// TestRefreshGitHubAppLoginsMintsAtTheInterval drives the loop with a
// millisecond interval rather than waiting out 25 minutes: the assertion is
// that every tick re-runs the login, which is what the ticker is for.
func TestRefreshGitHubAppLoginsMintsAtTheInterval(t *testing.T) {
	st, cfg, srv := seedCLIApp(t, "acme/app")
	defer srv.Close()

	var logins atomic.Int32
	restore := dockerLoginExec
	dockerLoginExec = func(context.Context, string, string, []byte) error {
		logins.Add(1)
		return nil
	}
	defer func() { dockerLoginExec = restore }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		refreshGitHubAppLogins(ctx, st, cfg, zerolog.Nop(), 2*time.Millisecond)
	}()

	deadline := time.After(5 * time.Second)
	for logins.Load() < 3 {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("only %d login(s) in 5s at a 2ms interval; the ticker is not firing", logins.Load())
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refreshGitHubAppLogins did not return after its context was cancelled")
	}
}

// TestRefreshGitHubAppLoginsWithoutAnAppIsANoOp: a node with no app keeps
// ticking, logs nothing and never reaches docker.
func TestRefreshGitHubAppLoginsWithoutAnAppIsANoOp(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	cfg := &config.Config{DataDir: dir}

	var logins atomic.Int32
	restore := dockerLoginExec
	dockerLoginExec = func(context.Context, string, string, []byte) error {
		logins.Add(1)
		return nil
	}
	defer func() { dockerLoginExec = restore }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		refreshGitHubAppLogins(ctx, st, cfg, zerolog.Nop(), time.Millisecond)
	}()
	// Let many ticks pass before cancelling; any login at all is a failure.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refreshGitHubAppLogins did not return after its context was cancelled")
	}
	if n := logins.Load(); n != 0 {
		t.Fatalf("an app-less node ran %d docker login(s); it must be a no-op", n)
	}
}

// TestServeStartsTheTickerAt25Minutes: the daemon starts the loop, exactly
// once, at the original two-logins-per-token interval. A second call site
// would double the mint rate; a different literal would leave a window where
// docker's config holds a credential the token cache has already outlived.
func TestServeStartsTheTickerAt25Minutes(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "serve.go"))
	if err != nil {
		t.Fatalf("read serve.go: %v", err)
	}
	body := string(src)
	if n := strings.Count(body, "go refreshGitHubAppLogins("); n != 1 {
		t.Fatalf("serve.go starts refreshGitHubAppLogins %d times, want exactly 1", n)
	}
	if !strings.Contains(body, "go refreshGitHubAppLogins(cmd.Context(), st, cfg, log, 25*time.Minute)") {
		t.Error("serve.go does not start the ticker at 25 minutes")
	}
}

// TestLeaderGainMintsOnceBeforeReconcile: on a follower→leader transition the
// credential must land before the reconcile writes any service spec, because
// --with-registry-auth freezes whatever docker holds at that moment.
func TestLeaderGainMintsOnceBeforeReconcile(t *testing.T) {
	st, cfg, srv := seedCLIApp(t, "acme/app")
	defer srv.Close()

	var logins atomic.Int32
	restore := dockerLoginExec
	dockerLoginExec = func(context.Context, string, string, []byte) error {
		logins.Add(1)
		return nil
	}
	defer func() { dockerLoginExec = restore }()

	var log logCapture
	var mu sync.Mutex
	var order []string
	leaderGainSteps(context.Background(),
		func(ctx context.Context) {
			loginOnLeadershipGain(ctx, st, cfg, log.logger())
			mu.Lock()
			order = append(order, "mint")
			mu.Unlock()
		},
		func() {
			mu.Lock()
			order = append(order, "reconcile")
			mu.Unlock()
		},
	)

	if n := logins.Load(); n != 1 {
		t.Fatalf("leadership gain ran %d docker login(s), want exactly 1", n)
	}
	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	if got != "mint,reconcile" {
		t.Fatalf("order = %s, want mint,reconcile", got)
	}
	if !log.contains("docker logged in to ghcr.io") {
		t.Errorf("no login warning captured; log:\n%s", log.text())
	}
}

// TestLoginWarnsWithTheExpiry: the operator asked to see the login. It must be
// a warning naming the registry, the expiry timestamp and the renewal cadence.
func TestLoginWarnsWithTheExpiry(t *testing.T) {
	st, cfg, srv := seedCLIApp(t, "acme/app")
	defer srv.Close()

	restore := dockerLoginExec
	dockerLoginExec = func(context.Context, string, string, []byte) error { return nil }
	defer func() { dockerLoginExec = restore }()

	var log logCapture
	if _, err := githubAppLogin(context.Background(), st, cfg, log.logger()); err != nil {
		t.Fatalf("githubAppLogin: %v", err)
	}

	entry := log.find("docker logged in to ghcr.io")
	if entry == nil {
		t.Fatalf("no login warning captured; log:\n%s", log.text())
	}
	if entry.Level != "warn" {
		t.Errorf("login logged at %q, want warn", entry.Level)
	}
	if !strings.Contains(entry.Message, "renewed every 25 minutes") {
		t.Errorf("warning does not name the renewal cadence: %q", entry.Message)
	}
	var expiry time.Time
	if raw, ok := entry.Fields["expires_at"]; ok {
		_ = json.Unmarshal(raw, &expiry)
	}
	if expiry.IsZero() {
		t.Fatalf("warning carries no usable expires_at field: %v", entry.Fields["expires_at"])
	}
	if !strings.Contains(entry.Message, expiry.UTC().Format(time.RFC3339)) {
		t.Errorf("warning text does not state the expiry %s: %q", expiry.UTC().Format(time.RFC3339), entry.Message)
	}
}

// TestLeadershipGainLoginFailureWarnsAndDoesNotAbort: a rejected docker login
// must not stop the reconcile — the deploy still gets a chance, and its pull
// error will name the registry problem.
func TestLeadershipGainLoginFailureWarnsAndDoesNotAbort(t *testing.T) {
	st, cfg, srv := seedCLIApp(t, "acme/app")
	defer srv.Close()

	restore := dockerLoginExec
	dockerLoginExec = func(context.Context, string, string, []byte) error {
		return errors.New("exit status 1: unauthorized: incorrect username or password")
	}
	defer func() { dockerLoginExec = restore }()

	var log logCapture
	reconciled := false
	leaderGainSteps(context.Background(),
		func(ctx context.Context) { loginOnLeadershipGain(ctx, st, cfg, log.logger()) },
		func() { reconciled = true },
	)
	if !reconciled {
		t.Fatal("a failed login aborted the reconcile")
	}
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

// TestRegistryLoginsSerialise: the timer, a leadership gain and a deploy-time
// login all write docker's config.json, so the exec must never overlap. The
// fake widens its own window; the assertion is observed concurrency.
func TestRegistryLoginsSerialise(t *testing.T) {
	st, cfg, srv := seedCLIApp(t, "acme/app")
	defer srv.Close()

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
			loginOnLeadershipGain(context.Background(), st, cfg, zerolog.Nop())
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

// TestNoSourceFileMentionsTheRemovedCredentialHelper is the inverse of the
// guard that once asserted the login timer was gone: the pull-time helper is
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

	exts := map[string]bool{
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
		if path == file || skip[path] || !exts[filepath.Ext(path)] {
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
