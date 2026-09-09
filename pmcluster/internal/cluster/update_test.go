package cluster

import (
	"context"
	"io"
	"path/filepath"
	"strings"
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

// TestUpdate_TLSNotACMEWithoutCertPathsErrorsIfNoACMEEmail reproduces the latent
// bug where `cluster update` keyed its TLS cert/ACME branch on state.Mode=="cert"
// while the infra-stack template keys on ACMEEmail emptiness. On a box whose TLS
// mode was never persisted (mode="", ACMEEmail="", no cert/key paths — the exact
// situation on the live box before the workaround), the old code silently loaded
// no cert/key, leaving __CERT_SECRET__/__KEY_SECRET__ empty while the template
// still rendered the `[[ if not .ACMEEmail ]]` block → a dangling `:` in the
// global secrets → invalid YAML on `cluster update`. The fix: an empty ACMEEmail
// ALWAYS requires cert/key paths, so update must fail loudly here rather than
// emit broken config.
func TestUpdate_TLSNotACMEWithoutCertPathsErrorsIfNoACMEEmail(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "tls.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cipher := openTestCipher(t, filepath.Join(dir, ".key"))

	ctx := context.Background()
	// Persist ONLY the domain — deliberately NO tls_mode / cert / key / acme
	// rows, mimicking a box that predates TLS-state persistence.
	if err := s.SetSetting(ctx, settingDomain, "test.example.com"); err != nil {
		t.Fatalf("persist domain: %v", err)
	}
	ct, err := cipher.Encrypt([]byte("pass"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := s.InsertCredential(ctx, &store.ManagedCredential{
		Name: "openobserve_admin", Kind: string(KindOpenObserve),
		Username: "ops@example.com", PasswordCiphertext: ct,
	}); err != nil {
		t.Fatalf("insert openobserve_admin: %v", err)
	}

	cfgDir := filepath.Join(dir, "config")
	if err := EnsureConfigDir(cfgDir, "v0.3.0"); err != nil {
		t.Fatalf("EnsureConfigDir: %v", err)
	}

	f := newFakeDocker()
	f.info = goodSwarmInfo()
	for _, name := range bundledServices {
		f.services[name] = healthyService(name)
	}

	_, err = Update(ctx, UpdateDeps{
		Store:    s,
		Cipher:   cipher,
		Docker:   f,
		Deployer: &recordingDeployer{},
		Stdout:   io.Discard,
	}, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"})
	if err == nil {
		t.Fatal("expected an error: ACMEEmail empty with no persisted cert/key paths must fail loudly, not emit invalid YAML")
	}
	if !strings.Contains(err.Error(), "no cert/key paths are persisted") {
		t.Errorf("error should explain the missing cert/key paths, got: %v", err)
	}
}

// TestUpdate_ACMEModeDoesNotRequireCertPaths ensures that a box in ACME mode
// (ACMEEmail persisted non-empty) updates without demanding cert/key paths and
// without a TLS error — update keys its branch on ACMEEmail exactly like the
// infra template, so the two never diverge.
func TestUpdate_ACMEModeDoesNotRequireCertPaths(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "tls.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cipher := openTestCipher(t, filepath.Join(dir, ".key"))

	ctx := context.Background()
	if err := s.SetSetting(ctx, settingDomain, "test.example.com"); err != nil {
		t.Fatalf("persist domain: %v", err)
	}
	if err := s.SetSetting(ctx, settingTLSMode, "acme"); err != nil {
		t.Fatalf("persist tls_mode: %v", err)
	}
	if err := s.SetSetting(ctx, settingTLSACME, "ops@example.com"); err != nil {
		t.Fatalf("persist acme email: %v", err)
	}
	ct, err := cipher.Encrypt([]byte("pass"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := s.InsertCredential(ctx, &store.ManagedCredential{
		Name: "openobserve_admin", Kind: string(KindOpenObserve),
		Username: "ops@example.com", PasswordCiphertext: ct,
	}); err != nil {
		t.Fatalf("insert openobserve_admin: %v", err)
	}

	cfgDir := filepath.Join(dir, "config")
	if err := EnsureConfigDir(cfgDir, "v0.3.0"); err != nil {
		t.Fatalf("EnsureConfigDir: %v", err)
	}

	f := newFakeDocker()
	f.info = goodSwarmInfo()
	for _, name := range bundledServices {
		f.services[name] = healthyService(name)
	}

	if _, err := Update(ctx, UpdateDeps{
		Store:    s,
		Cipher:   cipher,
		Docker:   f,
		Deployer: &recordingDeployer{},
		Stdout:   io.Discard,
	}, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("ACME-mode update should succeed without cert/key paths, got: %v", err)
	}
}
