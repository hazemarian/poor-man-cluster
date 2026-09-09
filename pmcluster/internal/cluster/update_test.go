package cluster

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

func openTestStore(t *testing.T, path string) (*store.Store, error) {
	t.Helper()
	return store.Open(path)
}

func openTestCipher(t *testing.T, path string) *credentials.Cipher {
	t.Helper()
	c, err := credentials.Open(path)
	if err != nil {
		t.Fatalf("credentials.Open: %v", err)
	}
	return c
}

func healthyService(name string) docker.Service {
	return docker.Service{Name: name, Replicas: 1, Desired: 1}
}

// seedUpdateState runs a real Up with a fresh fake docker + deployer, then
// returns a freshly-wired UpdateDeps (separate recording deployer) — plus the
// config dir shared by up and update — so update tests observe exactly what
// Update deploys against the persisted state + Docker.
func seedUpdateState(t *testing.T) (UpdateDeps, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "seed.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cipher := openTestCipher(t, filepath.Join(dir, ".key"))

	cfgDir := filepath.Join(dir, "config")
	if err := EnsureConfigDir(cfgDir, "v0.3.0"); err != nil {
		t.Fatalf("EnsureConfigDir: %v", err)
	}

	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	f := newFakeDocker()
	f.info = goodSwarmInfo()
	for _, name := range bundledServices {
		f.services[name] = healthyService(name)
	}

	if _, err := Up(context.Background(), UpDeps{
		Store:    s,
		Cipher:   cipher,
		Docker:   f,
		Deployer: &recordingDeployer{},
		Stdout:   io.Discard,
	}, UpInput{
		Domain:                "test.example.com",
		CertPath:              certPath,
		KeyPath:               keyPath,
		OpenObserveAdminEmail: "ops@example.com",
		ConfigDir:             cfgDir,
		Version:               "v0.3.0",
	}); err != nil {
		t.Fatalf("seed Up: %v", err)
	}

	return UpdateDeps{
		Store:    s,
		Cipher:   cipher,
		Docker:   f,
		Deployer: &recordingDeployer{},
		Stdout:   io.Discard,
	}, cfgDir
}

func TestUpdate_NoOpWhenNothingChanged(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	res, err := Update(context.Background(), deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	deployer := deps.Deployer.(*recordingDeployer)
	if len(deployer.deployedStacks) != 0 {
		t.Errorf("expected no stacks deployed on no-op, got %v", deployer.deployedStacks)
	}
	if res.OTelCreated || res.TraefikCreated || res.CertCreated || res.KeyCreated {
		t.Errorf("no-op update reported changes: %+v", res)
	}
}

func TestUpdate_ConfigsUseSeedVersionedNames(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	res, err := Update(context.Background(), deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	// After the seed up created pmcluster_otel_config_v001, an unchanged
	// update must reuse that same version rather than minting v002.
	if res.OTelConfig != "pmcluster_otel_config_v001" {
		t.Errorf("OTelConfig = %q, want pmcluster_otel_config_v001 (reused)", res.OTelConfig)
	}
	if res.TraefikConfig != "pmcluster_traefik_dynamic_v001" {
		t.Errorf("TraefikConfig = %q, want pmcluster_traefik_dynamic_v001 (reused)", res.TraefikConfig)
	}
}

// TestUpdate_PasswordRotationDoesNotRedeployObservability verifies the core
// decoupling: because the OTel collector authenticates with the STABLE root
// token (not the human password), rotating the OO password must NOT re-render
// the collector config nor re-deploy observability — that is the whole point
// of the token move.
func TestUpdate_PasswordRotationDoesNotRedeployObservability(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)

	ctx := context.Background()
	newPass, err := RandomPassword()
	if err != nil {
		t.Fatalf("random password: %v", err)
	}
	ct, err := deps.Cipher.Encrypt([]byte(newPass))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := deps.Store.RotateCredential(ctx, "openobserve_admin", ct); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	res, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if res.OTelCreated {
		t.Error("OTel config should NOT be re-created after a password rotation (collector uses the stable token)")
	}
	if res.OTelConfig != "pmcluster_otel_config_v001" {
		t.Errorf("OTelConfig = %q, want pmcluster_otel_config_v001 (unchanged)", res.OTelConfig)
	}
	deployer := deps.Deployer.(*recordingDeployer)
	if len(deployer.deployedStacks) != 0 {
		t.Errorf("expected NO stacks deployed on password-only rotation, got %v", deployer.deployedStacks)
	}
}
