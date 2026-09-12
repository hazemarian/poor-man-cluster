package cluster

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// newUpDeps returns a fully-wired UpDeps for unit tests.
// It uses a real store + cipher (temp files) and the fake docker + recording deployer.
func newUpDeps(t *testing.T) (UpDeps, *fakeDocker, *recordingDeployer) {
	t.Helper()
	dir := t.TempDir()

	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	c, err := credentials.Open(filepath.Join(dir, ".encryption_key"))
	if err != nil {
		t.Fatalf("credentials.Open: %v", err)
	}

	f := newFakeDocker()
	f.info = goodSwarmInfo()
	// Pre-seed bundled services as healthy so WaitHealthyStacks passes in
	// unit tests (real Docker not available). Each service reports 1/1 replicas.
	for _, name := range bundledServices {
		f.services[name] = docker.Service{Name: name, Replicas: 1, Desired: 1}
	}
	deployer := &recordingDeployer{}

	return UpDeps{
		Store:    s,
		Cipher:   c,
		Docker:   f,
		Deployer: deployer,
		Stdout:   io.Discard,
	}, f, deployer
}

// writeTempFile creates a small file with the given content and returns the path.
func writeTempFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
	return path
}

func TestUp_DeploysInCorrectOrder(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, _, deployer := newUpDeps(t)
	in := UpInput{
		Domain:                "test.example.com",
		CertPath:              certPath,
		KeyPath:               keyPath,
		OpenObserveAdminEmail: "ops@example.com",
	}

	res, err := Up(context.Background(), deps, in)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Four stacks must be deployed in the documented order (edge right after
	// infra so its route dependency comes up ASAP).
	wantOrder := []string{"infra", "edge", "observability", "backup"}
	if len(deployer.deployedStacks) != len(wantOrder) {
		t.Fatalf("deployed %d stacks, want %d: %v",
			len(deployer.deployedStacks), len(wantOrder), deployer.deployedStacks)
	}
	for i, want := range wantOrder {
		if deployer.deployedStacks[i].Name != want {
			t.Errorf("stack[%d] = %q, want %q", i, deployer.deployedStacks[i].Name, want)
		}
		if deployer.deployedStacks[i].YAMLLen == 0 {
			t.Errorf("stack[%d] %q: YAML length is 0", i, want)
		}
	}
	_ = res
}

func TestUp_IncludesCertKeyAndCredentialSecrets(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, _, _ := newUpDeps(t)
	in := UpInput{
		Domain:                "test.example.com",
		CertPath:              certPath,
		KeyPath:               keyPath,
		OpenObserveAdminEmail: "ops@example.com",
	}

	res, err := Up(context.Background(), deps, in)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	wantSecrets := map[string]bool{
		"cert_v001":                false,
		"key_v001":                 false,
		"admin_credentials":        false,
		"portainer_admin_password": false,
		"zo_root_user_password":    false,
	}
	for _, s := range res.NewSecrets {
		if _, ok := wantSecrets[s]; ok {
			wantSecrets[s] = true
		}
	}
	for name, seen := range wantSecrets {
		if !seen {
			t.Errorf("NewSecrets missing %q; got %v", name, res.NewSecrets)
		}
	}
}

func TestUp_ValidationError_MissingDomain(t *testing.T) {
	deps, f, deployer := newUpDeps(t)
	in := UpInput{
		Domain:                "", // missing
		CertPath:              "/some/cert",
		KeyPath:               "/some/key",
		OpenObserveAdminEmail: "ops@example.com",
	}

	_, err := Up(context.Background(), deps, in)
	if err == nil {
		t.Fatal("Up: expected error for missing domain, got nil")
	}

	// Docker must not have been touched.
	if len(f.networks) > 0 || len(f.secrets) > 0 || len(deployer.deployedStacks) > 0 {
		t.Error("Up touched docker resources despite validation failure")
	}
}

func TestUp_ValidationError_MissingCert(t *testing.T) {
	deps, _, _ := newUpDeps(t)
	_, err := Up(context.Background(), deps, UpInput{
		Domain:                "x.com",
		CertPath:              "",
		KeyPath:               "/key",
		OpenObserveAdminEmail: "ops@x.com",
	})
	if err == nil {
		t.Fatal("expected error for missing CertPath")
	}
}

func TestUp_ValidationError_MissingKey(t *testing.T) {
	deps, _, _ := newUpDeps(t)
	_, err := Up(context.Background(), deps, UpInput{
		Domain:                "x.com",
		CertPath:              "/cert",
		KeyPath:               "",
		OpenObserveAdminEmail: "ops@x.com",
	})
	if err == nil {
		t.Fatal("expected error for missing KeyPath")
	}
}

func TestUp_ValidationError_MissingOpenObserveEmail(t *testing.T) {
	deps, _, _ := newUpDeps(t)
	_, err := Up(context.Background(), deps, UpInput{
		Domain:                "x.com",
		CertPath:              "/cert",
		KeyPath:               "/key",
		OpenObserveAdminEmail: "",
	})
	if err == nil {
		t.Fatal("expected error for missing OpenObserveAdminEmail")
	}
}

func TestUp_PreflightFailure_TouchesNothing(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, f, deployer := newUpDeps(t)
	// Make Ping fail so preflight aborts immediately.
	f.pingErr = errSentinel

	_, err := Up(context.Background(), deps, UpInput{
		Domain:                "test.example.com",
		CertPath:              certPath,
		KeyPath:               keyPath,
		OpenObserveAdminEmail: "ops@example.com",
	})
	if err == nil {
		t.Fatal("Up: expected preflight error, got nil")
	}

	if len(f.networks) > 0 {
		t.Errorf("networks created despite preflight failure: %v", f.networks)
	}
	if len(f.secrets) > 0 {
		t.Errorf("secrets created despite preflight failure: %v", f.secrets)
	}
	if len(deployer.deployedStacks) > 0 {
		t.Errorf("stacks deployed despite preflight failure: %v", deployer.deployedStacks)
	}
}
