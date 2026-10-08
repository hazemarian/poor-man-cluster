package cluster

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
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

// TestUpdate_RedeploysWhenLiveServiceLabelDrifted covers BUG-017's drift
// detection: the stored rendered hash matches the fresh render, but a live
// service carries the rendered-hash label of a DIFFERENT render — a
// half-applied deploy, a skipped service, or an upgrade that never reached it.
// The DB-only comparison reported "nothing to redeploy" forever; the live
// comparison must redeploy. It also proves the inverse (matching labels → no
// redeploy) so the detector cannot simply always fire.
func TestUpdate_RedeploysWhenLiveServiceLabelDrifted(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()

	// Baseline: platform stacks deployed, stored hashes stamped.
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("baseline Update: %v", err)
	}

	f := deps.Docker.(*fakeDocker)
	row, err := deps.Store.GetConfig(ctx, "backup-stack")
	if err != nil {
		t.Fatalf("read backup-stack: %v", err)
	}
	// Recover the label the fresh render stamps from the stored render itself.
	var doc struct {
		Services map[string]struct {
			Deploy struct {
				Labels map[string]string `json:"labels"`
			} `json:"deploy"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal([]byte(row.RenderedContent), &doc); err != nil {
		t.Fatalf("parse stored render: %v", err)
	}
	freshHash := ""
	for _, s := range doc.Services {
		if h := s.Deploy.Labels[runtime.RenderedHashLabel]; h != "" {
			freshHash = h
		}
	}
	if freshHash == "" {
		t.Fatal("stored render carries no rendered-hash label")
	}

	// In sync: every live backup service carries the current label.
	for name, svc := range f.services {
		if strings.HasPrefix(name, "backup_") {
			svc.Labels = map[string]string{runtime.RenderedHashLabel: freshHash}
			f.services[name] = svc
		}
	}
	deployer := &recordingDeployer{}
	deps.Deployer = deployer
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("in-sync Update: %v", err)
	}
	if len(deployer.deployedStacks) != 0 {
		t.Fatalf("expected no stacks deployed when live labels match, got %v", deployer.deployedStacks)
	}

	// Drifted: one service still carries an older render's hash.
	svc := f.services["backup_control-plane-backup"]
	svc.Labels = map[string]string{runtime.RenderedHashLabel: "stale-from-an-older-render"}
	f.services["backup_control-plane-backup"] = svc

	deployer2 := &recordingDeployer{}
	deps.Deployer = deployer2
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("drifted Update: %v", err)
	}
	var backupDeployed bool
	for _, d := range deployer2.deployedStacks {
		if d.Name == "backup" {
			backupDeployed = true
		}
	}
	if !backupDeployed {
		t.Fatalf("expected the backup stack to be redeployed after label drift, got %v", deployer2.deployedStacks)
	}
}

// TestUpdate_RedeploysWhenServiceLabelMissing covers the PARTIALLY APPLIED
// deploy: some services in a stack carry the rendered-hash label while others
// do not — the shape left behind when a `stack deploy` is cut off mid-flight
// (e.g. the daemon restarts during an install). That mixed state must be
// treated as drift and re-applied.
func TestUpdate_RedeploysWhenServiceLabelMissing(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()

	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("baseline Update: %v", err)
	}
	f := deps.Docker.(*fakeDocker)
	row, err := deps.Store.GetConfig(ctx, "backup-stack")
	if err != nil {
		t.Fatalf("read backup-stack: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			Deploy struct {
				Labels map[string]string `json:"labels"`
			} `json:"deploy"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal([]byte(row.RenderedContent), &doc); err != nil {
		t.Fatalf("parse stored render: %v", err)
	}
	freshHash := ""
	for _, s := range doc.Services {
		if h := s.Deploy.Labels[runtime.RenderedHashLabel]; h != "" {
			freshHash = h
		}
	}
	if freshHash == "" {
		t.Fatal("stored render carries no rendered-hash label")
	}

	// Stamp every backup service except one: the interrupted-deploy shape.
	stamped := 0
	for name, svc := range f.services {
		if !strings.HasPrefix(name, "backup_") {
			continue
		}
		if name == "backup_volume-backup" {
			svc.Labels = map[string]string{}
		} else {
			svc.Labels = map[string]string{runtime.RenderedHashLabel: freshHash}
			stamped++
		}
		f.services[name] = svc
	}
	if stamped == 0 {
		t.Fatal("fixture stamped no services; the mixed state was not set up")
	}

	deployer := &recordingDeployer{}
	deps.Deployer = deployer
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("partial-deploy Update: %v", err)
	}
	var backupDeployed bool
	for _, d := range deployer.deployedStacks {
		if d.Name == "backup" {
			backupDeployed = true
		}
	}
	if !backupDeployed {
		t.Fatalf("expected the backup stack to be redeployed after a partial deploy, got %v", deployer.deployedStacks)
	}
}

func healthyService(name string) runtime.Service {
	// Derive the stack namespace from the service name ("infra_traefik" →
	// "infra") so the existence-aware reconcile loop can see which stacks are
	// present in the swarm.
	stack := name
	if i := strings.IndexByte(name, '_'); i >= 0 {
		stack = name[:i]
	}
	return runtime.Service{Name: name, Stack: stack, Replicas: 1, Desired: 1}
}

// seedUpdateState runs a real Up with a fresh fake docker + deployer, then
// returns a freshly-wired UpdateDeps (separate recording deployer) — plus the
// config dir shared by up and update — so update tests observe exactly what
// Update deploys against the persisted state + Docker.
func seedUpdateState(t *testing.T) (UpdateDeps, string) {
	t.Helper()
	dir := t.TempDir()
	ensureStorageDirs = func(string) error { return nil }
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
	seedHealthySwarm(f)

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

	// Reconstruct the post-deploy swarm state the fakeDocker's recording
	// deployer does not build on its own: every live service carries the
	// rendered-hash label the stored render stamped. Otherwise `cluster
	// update`'s drift check sees zero labelled services and (correctly)
	// redeploys every platform stack on a no-op update.
	stampRenderedHashLabels(t, s, f)

	return UpdateDeps{
		Store:    s,
		Cipher:   cipher,
		Docker:   f,
		Deployer: &recordingDeployer{},
		Stdout:   io.Discard,
	}, cfgDir
}

// TestUpdate_StorageNodeLabelsRepaired verifies the update step that keeps the
// pmcluster.storage node label in sync with the storage_nodes setting: every
// listed hostname gets the label (best-effort per-node).
func TestUpdate_StorageNodeLabelsRepaired(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()
	if err := deps.Store.SetSetting(ctx, SettingStorageNodes(), "nextrum-sy-1,nextrum-sy-2"); err != nil {
		t.Fatalf("SetSetting storage_nodes: %v", err)
	}
	// seed's fakeDocker has no nodes; give it the two hostnames.
	f := deps.Docker.(*fakeDocker)
	f.nodes = []runtime.Node{
		{Hostname: "nextrum-sy-1", ID: "n1", Role: "manager", Status: "ready", Availability: "active"},
		{Hostname: "nextrum-sy-2", ID: "n2", Role: "worker", Status: "ready", Availability: "active"},
	}

	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(f.nodeLabels) != 2 {
		t.Fatalf("expected 2 SetNodeLabel calls, got %v", f.nodeLabels)
	}
	for _, l := range f.nodeLabels {
		if l != runtime.StorageNodeLabel+"=true" {
			t.Errorf("unexpected label call %q", l)
		}
	}
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

// failingDeployer records deploys like recordingDeployer but fails any
// DeployStack whose stack name is in the failNames set.
type failingDeployer struct {
	deployedStacks []string
	failNames      map[string]bool
}

func (f *failingDeployer) DeployStack(_ context.Context, name string, composeYAML []byte) error {
	f.deployedStacks = append(f.deployedStacks, name)
	if f.failNames[name] {
		return errors.New("simulated deploy failure")
	}
	return nil
}

func (f *failingDeployer) DeployStackNoPrune(_ context.Context, name string, composeYAML []byte) error {
	f.deployedStacks = append(f.deployedStacks, name)
	if f.failNames[name] {
		return errors.New("simulated deploy failure")
	}
	return nil
}

func (f *failingDeployer) PruneStack(_ context.Context, name string, _ []byte) error { return nil }
func (f *failingDeployer) RemoveStack(_ context.Context, name string) error          { return nil }
func (f *failingDeployer) ForceUpdateService(_ context.Context, fullName string) error {
	return nil
}
func (f *failingDeployer) PruneStaleContainers(_ context.Context, _ string, _ string) error {
	return nil
}

// TestUpdate_FailedDeployLeavesStaleHashRetriedOnNextUpdate verifies BUG-009:
// a failed platform-stack deploy must NOT stamp the rendered hash, so the next
// `cluster update` retries the deploy instead of silently skipping it (the
// swarm never received the stack, but the old code marked it up-to-date).
func TestUpdate_FailedDeployLeavesStaleHashRetriedOnNextUpdate(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()

	// First update: everything is a no-op (fresh seed state) — record the
	// backup-stack rendered hash before we inject a failure.
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("baseline Update: %v", err)
	}
	row, err := deps.Store.GetConfig(ctx, string(StackBackup)+"-stack")
	if err != nil {
		t.Fatalf("GetConfig backup-stack: %v", err)
	}
	beforeHash := row.RenderedHash

	// Change the storage_nodes setting → the backup stack re-renders (new
	// storage-node constraint) → backup is redeployed. Make that deploy fail.
	if err := deps.Store.SetSetting(ctx, SettingStorageNodes(), "node-1"); err != nil {
		t.Fatalf("SetSetting storage_nodes: %v", err)
	}
	failing := &failingDeployer{failNames: map[string]bool{string(StackBackup): true}}
	deps.Deployer = failing

	_, err = Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"})
	if err == nil {
		t.Fatalf("expected Update to fail when backup deploy fails")
	}

	// The backup-stack hash must be UNCHANGED after the failed deploy.
	row, err = deps.Store.GetConfig(ctx, string(StackBackup)+"-stack")
	if err != nil {
		t.Fatalf("GetConfig backup-stack: %v", err)
	}
	if row.RenderedHash != beforeHash {
		t.Errorf("BUG-009: failed deploy stamped a new rendered hash (before=%q after=%q); next update would skip redeploy",
			beforeHash, row.RenderedHash)
	}

	// A second update (with the deploy now succeeding) must RETRY the backup
	// stack — proving the stale hash isn't blocking redeploy.
	deps.Deployer = &recordingDeployer{}
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("retry Update: %v", err)
	}
	deployer := deps.Deployer.(*recordingDeployer)
	found := false
	for _, rec := range deployer.deployedStacks {
		if rec.Name == string(StackBackup) {
			found = true
		}
	}
	if !found {
		t.Errorf("BUG-009: backup stack was NOT redeployed on the update after a failed deploy")
	}
}

// TestUpdate_EnablingSSOSelfHealsMissingCookieSecret verifies that enabling
// SSO on an existing cluster whose bootstrap predates the sso_cookie_secret
// credential self-heals: cluster update mints the credential + Swarm secret
// (idempotent Ensure) instead of erroring, and deploys the sso stack.
func TestUpdate_EnablingSSOSelfHealsMissingCookieSecret(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()

	// Simulate a cluster whose bootstrap predates the SSO credential: the
	// seed Up created it via Bootstrap, so remove the row + Swarm secret to
	// reproduce the legacy state cluster update must self-heal.
	if _, err := deps.Store.GetCredential(ctx, "sso_cookie_secret"); err != nil {
		t.Fatalf("seed should have bootstrapped sso_cookie_secret: %v", err)
	}
	if _, err := deps.Store.DB().ExecContext(ctx, "DELETE FROM managed_credentials WHERE name = ?", "sso_cookie_secret"); err != nil {
		t.Fatalf("delete sso_cookie_secret row: %v", err)
	}
	if err := deps.Docker.SecretRemove(ctx, "sso_cookie_secret"); err != nil {
		t.Fatalf("remove sso_cookie_secret swarm secret: %v", err)
	}

	for k, v := range map[string]string{
		SettingSSOEnabled():      "true",
		SettingSSOProvider():     "github",
		SettingSSOClientID():     "client-id",
		SettingSSOClientSecret(): "client-secret",
		SettingSSOGitHubOrg():    "nextrum-s",
	} {
		if err := deps.Store.SetSetting(ctx, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}

	res, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"})
	if err != nil {
		t.Fatalf("Update with SSO enabled: %v", err)
	}

	mc, err := deps.Store.GetCredential(ctx, "sso_cookie_secret")
	if err != nil {
		t.Fatalf("sso_cookie_secret credential should have been minted: %v", err)
	}
	if mc.SwarmSecretName != "sso_cookie_secret" {
		t.Errorf("SwarmSecretName = %q, want sso_cookie_secret", mc.SwarmSecretName)
	}

	deployer := deps.Deployer.(*recordingDeployer)
	var ssoDeployed bool
	for _, d := range deployer.deployedStacks {
		if d.Name == "sso" {
			ssoDeployed = true
			for _, want := range []string{"client-id", "client-secret", "nextrum-s", "sso_cookie_secret", "sso.test.example.com"} {
				if !strings.Contains(d.YAML, want) {
					t.Errorf("sso deploy YAML missing %q", want)
				}
			}
		}
	}
	if !ssoDeployed {
		t.Errorf("expected sso stack deployed, got %v", deployer.deployedStacks)
	}
	if !slices.Contains(res.StacksDeployed, "sso") {
		t.Errorf("UpdateResult.StacksDeployed should include sso, got %v", res.StacksDeployed)
	}

	// Second run: credential now exists, Ensure is a no-op, sso still converges.
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("second Update with SSO enabled: %v", err)
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
		if !strings.Contains(string(d.YAML), "ghcr.io/hazemarian/pmcluster-edge:v9.9.9") {
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

	// Content-addressed naming: the seeded OTel + Traefik configs are
	// materialized as <base>_<sha8> objects. Same content → same name → reuse
	// (no number is ever added).
	if !strings.HasPrefix(res.OTelConfig, "pmcluster_otel_config_") {
		t.Errorf("OTelConfig = %q, want pmcluster_otel_config_<sha8>", res.OTelConfig)
	}
	if !strings.HasPrefix(res.TraefikConfig, "pmcluster_traefik_dynamic_") {
		t.Errorf("TraefikConfig = %q, want pmcluster_traefik_dynamic_<sha8>", res.TraefikConfig)
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
	if res.OTelConfig == "pmcluster_otel_config_v1" {
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
	seedHealthySwarm(f)

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
	seedHealthySwarm(f)

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

// TestUpdate_AbsentStackRedeployedDespiteMatchingHash verifies the
// existence-aware reconcile: a stack whose rendered hash is unchanged but whose
// services are absent from the swarm (a wiped Swarm) is redeployed.
func TestUpdate_AbsentStackRedeployedDespiteMatchingHash(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()

	// Baseline no-op update: every stack present + hash matched.
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("baseline Update: %v", err)
	}

	// Simulate a wiped swarm: the backup stack's services are gone.
	f := deps.Docker.(*fakeDocker)
	for name := range f.services {
		if strings.HasPrefix(name, "backup_") {
			delete(f.services, name)
		}
	}

	deployer := &recordingDeployer{}
	deps.Deployer = deployer
	res, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	var backupDeployed bool
	for _, d := range deployer.deployedStacks {
		if d.Name == "backup" {
			backupDeployed = true
		}
	}
	if !backupDeployed {
		t.Errorf("absent backup stack was not redeployed despite matching hash: %v", deployer.deployedStacks)
	}
	if !slices.Contains(res.StacksDeployed, "backup") {
		t.Errorf("StacksDeployed should include backup, got %v", res.StacksDeployed)
	}
}

// TestUpdate_ReEnsuresNetworks verifies `cluster update` re-creates the
// external overlay networks that only `cluster up` used to ensure.
func TestUpdate_ReEnsuresNetworks(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()

	f := deps.Docker.(*fakeDocker)
	delete(f.networks, "traefik-net")
	delete(f.networks, "monitoring-net")

	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	for _, name := range []string{"traefik-net", "monitoring-net"} {
		if _, ok := f.networks[name]; !ok {
			t.Errorf("network %s was not re-ensured by update", name)
		}
	}
}

// TestUpdate_MissingManagedCredentialSecretRecreated verifies update
// re-materializes a managed-credential Swarm secret from the DB ciphertext
// before the stack reconcile, without rotating the credential.
func TestUpdate_MissingManagedCredentialSecretRecreated(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()

	before, err := deps.Store.GetCredential(ctx, "traefik_dashboard")
	if err != nil {
		t.Fatalf("GetCredential traefik_dashboard: %v", err)
	}

	if err := deps.Docker.SecretRemove(ctx, "admin_credentials"); err != nil {
		t.Fatalf("remove admin_credentials: %v", err)
	}

	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	f := deps.Docker.(*fakeDocker)
	if _, ok := f.secrets["admin_credentials"]; !ok {
		t.Fatal("admin_credentials swarm secret was not re-materialized by update")
	}

	after, err := deps.Store.GetCredential(ctx, "traefik_dashboard")
	if err != nil {
		t.Fatalf("GetCredential traefik_dashboard (after): %v", err)
	}
	if !bytes.Equal(before.PasswordCiphertext, after.PasswordCiphertext) {
		t.Error("credential ciphertext changed (rotated) — materialization must never rotate")
	}
}

// TestUpdate_SwarmIDChangeForcesRedeploy verifies a changed (or newly-seen)
// Swarm cluster ID force-redeploys every platform stack and persists the ID.
func TestUpdate_SwarmIDChangeForcesRedeploy(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()

	f := deps.Docker.(*fakeDocker)
	f.swarmID = "swarm-v2"
	deployer := &recordingDeployer{}
	deps.Deployer = deployer

	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("Update after swarm ID change: %v", err)
	}

	got := map[string]bool{}
	for _, d := range deployer.deployedStacks {
		got[d.Name] = true
	}
	for _, s := range []string{"observability", "infra", "edge", "backup"} {
		if !got[s] {
			t.Errorf("swarm-ID change should force redeploy of %s, got %v", s, deployer.deployedStacks)
		}
	}
	if v := deps.Store.GetSettingDefault(ctx, settingSwarmID, ""); v != "swarm-v2" {
		t.Errorf("swarm_id setting = %q, want swarm-v2", v)
	}

	// Second update with the same ID: no change → no redeploy.
	deployer2 := &recordingDeployer{}
	deps.Deployer = deployer2
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("second Update: %v", err)
	}
	if len(deployer2.deployedStacks) != 0 {
		t.Errorf("expected no redeploy on unchanged swarm ID, got %v", deployer2.deployedStacks)
	}
}

// TestUpdate_ForceRedeploysAllStacks verifies the `cluster reset` force mode:
// every platform stack is redeployed regardless of hash or existence.
func TestUpdate_ForceRedeploysAllStacks(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)

	deployer := &recordingDeployer{}
	deps.Deployer = deployer

	if _, err := Update(context.Background(), deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0", Force: true}); err != nil {
		t.Fatalf("Update force: %v", err)
	}

	got := map[string]bool{}
	for _, d := range deployer.deployedStacks {
		got[d.Name] = true
	}
	for _, s := range []string{"observability", "infra", "edge", "backup"} {
		if !got[s] {
			t.Errorf("force update should redeploy %s, got %v", s, deployer.deployedStacks)
		}
	}
}

// TestUpdate_AddsLeaderToStorageNodes verifies the BUG-024 policy: the leader
// must be a storage node (the backup agent and the edge run there), so an
// update that finds storage_nodes without the leader re-adds it and labels it.
func TestUpdate_AddsLeaderToStorageNodes(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()
	if err := deps.Store.SetSetting(ctx, SettingStorageNodes(), "nextrum-sy-2"); err != nil {
		t.Fatalf("SetSetting storage_nodes: %v", err)
	}
	f := deps.Docker.(*fakeDocker)
	f.nodes = []runtime.Node{
		{Hostname: "nextrum-sy-1", ID: "n1", Role: "manager", Status: "ready", Availability: "active", IsLeader: true},
		{Hostname: "nextrum-sy-2", ID: "n2", Role: "worker", Status: "ready", Availability: "active"},
	}
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := deps.Store.GetSettingDefault(ctx, SettingStorageNodes(), "")
	if !strings.Contains(got, "nextrum-sy-1") {
		t.Fatalf("the leader must be added to storage_nodes, got %q", got)
	}
	if len(f.nodeLabels) != 2 {
		t.Fatalf("expected the label for both storage nodes, got %v", f.nodeLabels)
	}
}

// TestUpdate_DemotesFormerLeader verifies that storage follows leadership: when
// leadership moves, the new leader is a storage node and the FORMER leader stops
// being one (its storage label is cleared).
func TestUpdate_DemotesFormerLeader(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()
	if err := deps.Store.SetSetting(ctx, SettingStorageNodes(), "node-a,node-b"); err != nil {
		t.Fatalf("SetSetting storage_nodes: %v", err)
	}
	if err := deps.Store.SetSetting(ctx, storageLeaderKey, "node-a"); err != nil {
		t.Fatalf("SetSetting storage_leader: %v", err)
	}
	f := deps.Docker.(*fakeDocker)
	f.nodes = []runtime.Node{
		{Hostname: "node-a", ID: "na", Role: "manager", Status: "ready", Availability: "active"},
		{Hostname: "node-b", ID: "nb", Role: "manager", Status: "ready", Availability: "active", IsLeader: true},
	}
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := deps.Store.GetSettingDefault(ctx, SettingStorageNodes(), "")
	if strings.Contains(got, "node-a") {
		t.Fatalf("the former leader must be demoted, storage_nodes = %q", got)
	}
	if !strings.Contains(got, "node-b") {
		t.Fatalf("the new leader must stay a storage node, storage_nodes = %q", got)
	}
	joined := strings.Join(f.nodeLabels, ",")
	if !strings.Contains(joined, runtime.StorageNodeLabel+"=true") {
		t.Fatalf("the new leader must be labeled, calls = %v", f.nodeLabels)
	}
	if !strings.Contains(joined, runtime.StorageNodeLabel+"=") {
		t.Fatalf("the former leader's label must be cleared, calls = %v", f.nodeLabels)
	}
	if leader := deps.Store.GetSettingDefault(ctx, storageLeaderKey, ""); leader != "node-b" {
		t.Fatalf("storage_leader = %q, want node-b", leader)
	}
}
