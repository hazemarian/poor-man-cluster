package cluster

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/buildinfo"
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
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
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

// TestUpdate_VersionBumpRedeploysEdgeWithPinnedTag verifies the edge stack
// pins the pmcluster release tag by default, so a version bump re-renders the
// edge stack with the new tag and re-deploys the edge. This keeps the edge
// image in lock-step with the binary (no more stale :latest on nodes). Since
// the config templates are stamped with the build version, a version bump also
// rotates the OTel + Traefik configs → observability + infra re-deploy too.
func TestUpdate_VersionBumpRedeploysEdgeWithPinnedTag(t *testing.T) {
	t.Setenv(EdgeImageEnv, "")
	deps, cfgDir := seedUpdateState(t)

	origVersion := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = origVersion })
	buildinfo.Version = "v0.3.1"

	res, err := Update(context.Background(), deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.1"})
	if err != nil {
		t.Fatalf("bump Update: %v", err)
	}
	if !res.EdgeCreated {
		t.Error("version bump should re-render the edge stack (new pinned tag → EdgeCreated)")
	}
	if !res.EdgeDeployed {
		t.Error("version bump should re-deploy the edge with the new pinned tag")
	}
	if res.OTelCreated || res.TraefikCreated {
		t.Error("version bump must NOT rotate OTel + Traefik configs (no version stamping — content is hash-compared)")
	}
	deployer := deps.Deployer.(*recordingDeployer)
	got := make([]string, 0, len(deployer.deployedStacks))
	for _, d := range deployer.deployedStacks {
		got = append(got, d.Name)
	}
	want := []string{"edge"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("version bump should re-deploy %v (edge image repinned, configs content-stable), got %v", want, got)
	}
}

// TestUpdate_EdgeImagePinRedeploysEdge verifies setting PMCLUSTER_EDGE_IMAGE
// (the way an operator ships a new edge release) changes the rendered edge
// compose → a new edge content fingerprint → an edge-only redeploy (no version
// bump in play, so the OTel/Traefik configs are untouched).
func TestUpdate_EdgeImagePinRedeploysEdge(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)

	t.Setenv(EdgeImageEnv, "")
	noopUp := &recordingDeployer{}
	deps.Deployer = noopUp
	if _, err := Update(context.Background(), deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("no-op Update: %v", err)
	}
	if len(noopUp.deployedStacks) != 0 {
		t.Fatalf("expected no deploy on no-op update, got %v", noopUp.deployedStacks)
	}

	t.Setenv(EdgeImageEnv, "v9.9.9")
	pinUp := &recordingDeployer{}
	deps.Deployer = pinUp
	res, err := Update(context.Background(), deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"})
	if err != nil {
		t.Fatalf("pin Update: %v", err)
	}
	if !res.EdgeCreated {
		t.Error("expected edge content fingerprint to change when the edge image is pinned (EdgeCreated)")
	}
	if !res.EdgeDeployed {
		t.Error("expected EdgeDeployed=true when the edge image is pinned")
	}
	if len(pinUp.deployedStacks) != 1 || pinUp.deployedStacks[0].Name != "edge" {
		t.Errorf("expected only the edge stack re-deployed on image pin, got %v", pinUp.deployedStacks)
	}
	for _, d := range pinUp.deployedStacks {
		if !strings.Contains(string(d.YAML), "ghcr.io/nextrum-sy/pmcluster-edge:v9.9.9") {
			t.Errorf("edge stack should pin image :v9.9.9, got:\n%s", d.YAML)
		}
	}
}

func TestUpdate_ConfigsUseSeedVersionedNames(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	res, err := Update(context.Background(), deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if res.OTelConfig != "pmcluster_otel_config_v001" {
		t.Errorf("OTelConfig = %q, want pmcluster_otel_config_v001 (reused)", res.OTelConfig)
	}
	if res.TraefikConfig != "pmcluster_traefik_dynamic_v001" {
		t.Errorf("TraefikConfig = %q, want pmcluster_traefik_dynamic_v001 (reused)", res.TraefikConfig)
	}
}

// TestUpdate_PasswordRotationRedeploysObservabilityAndInfra verifies the
// render-level semantics: the OTel collector config authenticates with the ROOT
// admin credentials (Basic base64(email:password)) — the same ones OpenObserve
// itself runs with — so a password rotation DOES re-create the OTel config and
// re-deploy observability. The observability stack compose embeds
// ZO_ROOT_USER_PASSWORD directly AND the traefik-dynamic config embeds the OO
// root credentials in the openobserve-auto-auth middleware, so both stacks'
// rendered content changes and BOTH observability + infra are re-deployed —
// applying the new password to the running service and to the Traefik-injected
// Authorization header.
func TestUpdate_PasswordRotationRedeploysObservabilityAndInfra(t *testing.T) {
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
	if !res.OTelCreated {
		t.Error("OTel config should be re-created after a password rotation (collector embeds the admin basic auth)")
	}
	if res.OTelConfig == "pmcluster_otel_config_v001" {
		t.Error("OTelConfig should have been minted to a new version after rotation")
	}
	deployer := deps.Deployer.(*recordingDeployer)
	if len(deployer.deployedStacks) != 2 {
		t.Fatalf("expected observability + infra re-deployed (password env + auto-auth middleware changed), got %v", deployer.deployedStacks)
	}
	if !res.TraefikCreated {
		t.Error("traefik-dynamic config should be re-created (openobserve-auto-auth middleware embeds the NEW password)")
	}
	var observability, infra string
	for _, d := range deployer.deployedStacks {
		switch d.Name {
		case "observability":
			observability = d.YAML
		case "infra":
			infra = d.YAML
		}
	}
	if observability == "" {
		t.Fatal("observability was not re-deployed")
	}
	if infra == "" {
		t.Fatal("infra was not re-deployed")
	}
	// LoadComposeFile escapes '$' as '$$' for Docker Compose variable
	// safety (escapeComposeDollar), so assert on the escaped form —
	// a raw "$" in a random password would otherwise flake the match.
	if !strings.Contains(string(observability), escapeComposeDollar(newPass)) {
		t.Errorf("observability re-deploy must carry the NEW password in ZO_ROOT_USER_PASSWORD")
	}
}

// TestUpdate_TLSNotACMEWithoutCertPathsErrorsIfNoACMEEmail reproduces the latent
// bug where `cluster update` keyed its TLS cert/ACME branch on state.Mode=="cert"
// while the infra-stack template keys on ACMEEmail emptiness. On a box whose TLS
// mode was never persisted (mode="", ACMEEmail="", no cert/key paths — the exact
// situation on the live box before the workaround), the old code silently loaded
// no cert/key, leaving secrets(cert)/secrets(key) empty while the template
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
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
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
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
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
