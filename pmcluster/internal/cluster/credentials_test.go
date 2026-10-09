package cluster

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// newTestDeps returns a real *store.Store and *credentials.Cipher backed by
// temp files. Both are fast and involve no network I/O.
func newTestDeps(t *testing.T) (*store.Store, *credentials.Cipher) {
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
	return s, c
}

func TestBootstrap_RequiresOpenObserveEmail(t *testing.T) {
	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()

	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f}
	_, err := mgr.Bootstrap(context.Background(), BootstrapInput{
		TraefikAdminUser:      "admin",
		OpenObserveAdminEmail: "",
	})
	if err == nil {
		t.Fatal("Bootstrap: expected error when OpenObserveAdminEmail is empty")
	}
}

func TestBootstrap_DefaultsTraefikAdminUser(t *testing.T) {
	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()

	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f}
	creds, err := mgr.Bootstrap(context.Background(), BootstrapInput{
		TraefikAdminUser:      "",
		OpenObserveAdminEmail: "ops@example.com",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	traefik, ok := creds["traefik_dashboard"]
	if !ok {
		t.Fatal("traefik_dashboard credential missing from result")
	}
	if traefik.Username != "admin" {
		t.Errorf("Username = %q, want 'admin'", traefik.Username)
	}
}

func TestBootstrap_ReturnsAllCredentials(t *testing.T) {
	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()

	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f}
	creds, err := mgr.Bootstrap(context.Background(), BootstrapInput{
		OpenObserveAdminEmail: "ops@example.com",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	wantNames := []string{
		"traefik_dashboard", "openobserve_admin",
		"edge_admin", "edge_ui_secret", "edge_api_token",
	}
	for _, name := range wantNames {
		if _, ok := creds[name]; !ok {
			t.Errorf("credential %q missing from result", name)
		}
	}
}

// TestBootstrap_EdgeAPIToken verifies the dedicated daemon user "edge" is
// created for the console's API token, that the minted token clears
// auth.VerifyToken against the stored hash, and that all three edge Swarm
// secrets are provisioned.
func TestBootstrap_EdgeAPIToken(t *testing.T) {
	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()

	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f}
	ctx := context.Background()
	creds, err := mgr.Bootstrap(ctx, BootstrapInput{OpenObserveAdminEmail: "ops@example.com"})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	token := creds["edge_api_token"].Password
	user, err := s.UserByToken(ctx, token)
	if err != nil {
		t.Fatalf("daemon lookup should accept the edge token: %v", err)
	}
	if user.Name != "edge" {
		t.Errorf("daemon authenticated user = %q, want %q", user.Name, "edge")
	}

	for name, secret := range map[string]string{
		"edge_admin_password": "edge_admin",
		"edge_ui_secret":      "edge_ui_secret",
		"edge_api_token":      "edge_api_token",
	} {
		spec, ok := f.secrets[name]
		if !ok {
			t.Errorf("edge swarm secret %q not created", name)
			continue
		}
		if string(spec.Data) != creds[secret].Password {
			t.Errorf("secret %q payload mismatch with credential %q", name, secret)
		}
	}
}

func TestBootstrap_FreshInstall_AllNewlyCreated(t *testing.T) {
	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()

	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f}
	creds, err := mgr.Bootstrap(context.Background(), BootstrapInput{
		OpenObserveAdminEmail: "ops@example.com",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for name, cr := range creds {
		if !cr.NewlyCreated {
			t.Errorf("%s: NewlyCreated = false, want true on first run", name)
		}
		if !cr.SwarmSecretCreated {
			t.Errorf("%s: SwarmSecretCreated = false, want true on first run", name)
		}
	}
}

func TestBootstrap_ReRun_NotNewlyCreated(t *testing.T) {
	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()

	in := BootstrapInput{OpenObserveAdminEmail: "ops@example.com"}
	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f}

	first, err := mgr.Bootstrap(context.Background(), in)
	if err != nil {
		t.Fatalf("Bootstrap (first): %v", err)
	}

	second, err := mgr.Bootstrap(context.Background(), in)
	if err != nil {
		t.Fatalf("Bootstrap (second): %v", err)
	}

	for name, cr := range second {
		if cr.NewlyCreated {
			t.Errorf("%s: NewlyCreated = true on re-run, want false", name)
		}
		if cr.SwarmSecretCreated {
			t.Errorf("%s: SwarmSecretCreated = true on re-run, want false", name)
		}

		if cr.Password != first[name].Password {
			t.Errorf("%s: password changed between runs", name)
		}
	}
}

func TestBootstrap_TraefikSecretIsHtpasswd(t *testing.T) {
	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()

	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f}
	_, err := mgr.Bootstrap(context.Background(), BootstrapInput{
		OpenObserveAdminEmail: "ops@example.com",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	spec, ok := f.secrets["admin_credentials"]
	if !ok {
		t.Fatal("admin_credentials secret not found in fake docker")
	}
	if !strings.HasPrefix(string(spec.Data), "admin:") {
		t.Errorf("admin_credentials payload = %q, want 'admin:...' htpasswd format", spec.Data)
	}
}

func TestBootstrap_OpenObserveSecretIsPlaintext(t *testing.T) {
	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()

	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f}
	creds, err := mgr.Bootstrap(context.Background(), BootstrapInput{
		OpenObserveAdminEmail: "ops@example.com",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	ooCred := creds["openobserve_admin"]
	ooSpec, ok := f.secrets["zo_root_user_password"]
	if !ok {
		t.Fatal("zo_root_user_password secret not found in fake docker")
	}
	if string(ooSpec.Data) != ooCred.Password {
		t.Errorf("openobserve secret payload = %q, want plaintext password %q",
			ooSpec.Data, ooCred.Password)
	}
}

// TestBootstrap_LostDBRecovery simulates "fresh store, but Swarm already has
// the secrets" — the divergence path where NewlyCreated=true but
// SwarmSecretCreated=false.
func TestBootstrap_LostDBRecovery(t *testing.T) {
	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()

	f.secrets["admin_credentials"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "admin_credentials", Data: []byte("admin:oldhash\n")}
	f.secrets["zo_root_user_password"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "zo_root_user_password", Data: []byte("oldoopass")}
	f.secrets["zo_root_user_token"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "zo_root_user_token", Data: []byte("oldtoken")}
	f.secrets["edge_admin_password"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "edge_admin_password", Data: []byte("oldadminpass")}
	f.secrets["edge_ui_secret"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "edge_ui_secret", Data: []byte("olduisecret")}
	f.secrets["edge_api_token"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "edge_api_token", Data: []byte("oldapitoken")}
	f.secrets["sso_cookie_secret"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "sso_cookie_secret", Data: []byte("oldssosecret")}
	f.secrets["seaweedfs_credentials"] = struct {
		Name   string
		Data   []byte
		Labels map[string]string
	}{Name: "seaweedfs_credentials", Data: []byte("oldseaweedpass")}

	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f}
	creds, err := mgr.Bootstrap(context.Background(), BootstrapInput{
		OpenObserveAdminEmail: "ops@example.com",
	})
	if err != nil {
		t.Fatalf("Bootstrap (lost-DB): %v", err)
	}

	for name, cr := range creds {
		if !cr.NewlyCreated {
			t.Errorf("%s: NewlyCreated = false, want true (DB was fresh)", name)
		}
		if cr.SwarmSecretCreated {
			t.Errorf("%s: SwarmSecretCreated = true, want false (secret pre-existed)", name)
		}
	}
}

// bootstrapForRotate is a shared helper that bootstraps all three credentials
// and returns the manager + the fakeDocker for use in Rotate tests.
func bootstrapForRotate(t *testing.T) (*CredentialsManager, *fakeDocker, *recordingDeployer) {
	t.Helper()
	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()
	rec := &recordingDeployer{}

	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f, Deployer: rec}
	_, err := mgr.Bootstrap(context.Background(), BootstrapInput{
		OpenObserveAdminEmail: "ops@example.com",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return mgr, f, rec
}

func TestRotate_HappyPath(t *testing.T) {
	mgr, f, rec := bootstrapForRotate(t)
	ctx := context.Background()

	originalCred, err := mgr.Store.GetCredential(ctx, "traefik_dashboard")
	if err != nil {
		t.Fatalf("GetCredential (before rotate): %v", err)
	}
	originalCiphertext := make([]byte, len(originalCred.PasswordCiphertext))
	copy(originalCiphertext, originalCred.PasswordCiphertext)

	oldSecretData, ok := f.secrets["admin_credentials"]
	if !ok {
		t.Fatal("admin_credentials secret should exist after Bootstrap")
	}

	newCred, err := mgr.Rotate(ctx, "traefik_dashboard")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	if newCred.Name != "traefik_dashboard" {
		t.Errorf("Name = %q, want 'traefik_dashboard'", newCred.Name)
	}
	if newCred.Username != originalCred.Username {
		t.Errorf("Username changed: %q → %q", originalCred.Username, newCred.Username)
	}
	if !strings.HasPrefix(newCred.SwarmSecretName, "admin_credentials_") {
		t.Errorf("SwarmSecretName = %q, want a content-addressed admin_credentials_<sha8>", newCred.SwarmSecretName)
	}
	if newCred.SwarmSecretCreated != true {
		t.Error("SwarmSecretCreated should be true after Rotate")
	}
	if newCred.NewlyCreated != false {
		t.Error("NewlyCreated should be false after Rotate (credential already existed)")
	}
	if newCred.Password == "" {
		t.Error("returned Password should be non-empty")
	}

	updatedCred, err := mgr.Store.GetCredential(ctx, "traefik_dashboard")
	if err != nil {
		t.Fatalf("GetCredential (after rotate): %v", err)
	}
	if string(updatedCred.PasswordCiphertext) == string(originalCiphertext) {
		t.Error("PasswordCiphertext should have changed after Rotate")
	}
	if updatedCred.SwarmSecretName != newCred.SwarmSecretName {
		t.Errorf("stored SwarmSecretName = %q, want %q", updatedCred.SwarmSecretName, newCred.SwarmSecretName)
	}

	// The OLD secret must remain untouched — never deleted while still mounted.
	oldAfter, ok := f.secrets["admin_credentials"]
	if !ok {
		t.Fatal("admin_credentials secret must still exist after Rotate (never deleted)")
	}
	if string(oldAfter.Data) != string(oldSecretData.Data) {
		t.Error("old swarm secret payload must be unchanged after Rotate (never replaced in place)")
	}

	// The NEW content-addressed secret must exist with the new value.
	newSecretData, ok := f.secrets[newCred.SwarmSecretName]
	if !ok {
		t.Fatalf("new swarm secret %s should exist after Rotate", newCred.SwarmSecretName)
	}
	if string(newSecretData.Data) == string(oldSecretData.Data) {
		t.Error("new swarm secret payload should differ from the old one")
	}

	found := false
	for _, svc := range rec.forceUpdated {
		if svc == "infra_traefik" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'infra_traefik' in forceUpdated, got %v", rec.forceUpdated)
	}
}

// TestRotate_RefusesEdgeAPIToken ensures the edge console's daemon token is
// not rotatable through the generic path (rotating the stored value without
// rewriting the "edge" user's hash would break console auth).
func TestRotate_RefusesEdgeAPIToken(t *testing.T) {
	mgr, _, _ := bootstrapForRotate(t)
	_, err := mgr.Rotate(context.Background(), "edge_api_token")
	if err == nil {
		t.Fatal("Rotate of edge_api_token should be refused")
	}
	if !strings.Contains(err.Error(), "cannot rotate") {
		t.Errorf("error %q should say 'cannot rotate'", err.Error())
	}
}

func TestRotate_UnknownCredential(t *testing.T) {
	mgr, _, _ := bootstrapForRotate(t)
	ctx := context.Background()

	_, err := mgr.Rotate(ctx, "nonexistent_cred")
	if err == nil {
		t.Fatal("Rotate of unknown credential should return an error")
	}
	if !errors.Is(err, store.ErrCredentialNotFound) {
		t.Errorf("err = %v, want to wrap ErrCredentialNotFound", err)
	}
}

func TestRotate_SecretRemoveFails(t *testing.T) {
	mgr, f, _ := bootstrapForRotate(t)
	ctx := context.Background()

	// Rotation must NEVER remove a secret that a running service may still
	// mount — the old secret stays and the new value lands under a fresh
	// content-addressed name. Inject a SecretRemove failure to prove Remove is
	// not even called.
	injectErr := errors.New("secret is in use by a running service")
	f.secretRemoveErr = map[string]error{
		"admin_credentials": injectErr,
	}

	if _, err := mgr.Rotate(ctx, "traefik_dashboard"); err != nil {
		t.Fatalf("Rotate must not fail on a would-be secret remove: %v", err)
	}
	if len(f.removedSecrets) != 0 {
		t.Fatalf("Rotate must never remove a secret; removed %v", f.removedSecrets)
	}
	if _, ok := f.secrets["admin_credentials"]; !ok {
		t.Fatal("the old admin_credentials secret must remain after Rotate")
	}
}

// TestRotate_SecretCreateFailsLeavesStoreUntouched verifies the atomicity of the
// split-brain fix: the NEW value is materialized into a fresh Swarm secret
// FIRST, and only then is the store row updated. When that first create fails,
// the store row must be left completely unchanged (the old code updated the
// store BEFORE touching Swarm, so a failure left them diverged).
func TestRotate_SecretCreateFailsLeavesStoreUntouched(t *testing.T) {
	mgr, f, _ := bootstrapForRotate(t)
	ctx := context.Background()

	before, err := mgr.Store.GetCredential(ctx, "traefik_dashboard")
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}

	f.secretCreateErr = errSentinel

	if _, err := mgr.Rotate(ctx, "traefik_dashboard"); err == nil {
		t.Fatal("Rotate should fail when the new secret cannot be created")
	}

	after, err := mgr.Store.GetCredential(ctx, "traefik_dashboard")
	if err != nil {
		t.Fatalf("GetCredential after failed rotate: %v", err)
	}
	if string(before.PasswordCiphertext) != string(after.PasswordCiphertext) {
		t.Error("store row must be unchanged when the new secret cannot be created first")
	}
	if before.SwarmSecretName != after.SwarmSecretName {
		t.Error("store swarm secret name must be unchanged when the new secret cannot be created first")
	}
	if len(f.removedSecrets) != 0 {
		t.Errorf("no secret should be removed on a failed rotate; removed %v", f.removedSecrets)
	}
}

func TestRotate_SpecNotFound_ReturnsError(t *testing.T) {

	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()
	rec := &recordingDeployer{}

	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f, Deployer: rec}
	ctx := context.Background()

	password := []byte("some-random-password")
	ciphertext, err := c.Encrypt(password)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if err := f.SecretCreate(ctx, runtime.SecretSpec{Name: "unknown_swarm_secret", Data: password}); err != nil {
		t.Fatalf("SecretCreate: %v", err)
	}
	if err := s.InsertCredential(ctx, &store.ManagedCredential{
		Name:               "unknown_thing",
		Kind:               "custom",
		Username:           "user",
		PasswordCiphertext: ciphertext,
		SwarmSecretName:    "unknown_swarm_secret",
	}); err != nil {
		t.Fatalf("InsertCredential: %v", err)
	}

	_, err = mgr.Rotate(ctx, "unknown_thing")
	if err == nil {
		t.Fatal("Rotate should return error when credential kind has no spec")
	}

	if !strings.Contains(err.Error(), "no spec for credential kind") {
		t.Errorf("expected 'no spec for credential kind' in error, got: %v", err)
	}
}

// TestRotate_SyncsDbSecret ensures the console reveal source (the daemon DB
// secrets row, name == SwarmSecretName) is updated to the new password too, so
// GET /api/secrets/{name}/value keeps matching the rotated credential.
func TestRotate_SyncsDbSecret(t *testing.T) {
	mgr, f, _ := bootstrapForRotate(t)
	ctx := context.Background()

	orig, err := mgr.Store.GetCredential(ctx, "openobserve_admin")
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}

	if _, err := mgr.Store.CreateSecret(ctx, "cluster", "", "zo_root_user_password",
		orig.PasswordCiphertext, store.SecretHash("old-password")); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	oldSecretData := f.secrets["zo_root_user_password"].Data

	newCred, err := mgr.Rotate(ctx, "openobserve_admin")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	cred, err := mgr.Store.GetCredential(ctx, "openobserve_admin")
	if err != nil {
		t.Fatalf("GetCredential after Rotate: %v", err)
	}
	plain, err := mgr.Cipher.Decrypt(cred.PasswordCiphertext)
	if err != nil {
		t.Fatalf("decrypt credential: %v", err)
	}
	if string(plain) != newCred.Password {
		t.Errorf("credential password = %q, want %q", plain, newCred.Password)
	}

	sec, err := mgr.Store.GetSecret(ctx, "zo_root_user_password")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	secPlain, err := mgr.Cipher.Decrypt(sec.Payload)
	if err != nil {
		t.Fatalf("decrypt db secret: %v", err)
	}
	if string(secPlain) != newCred.Password {
		t.Errorf("db secret value = %q, want %q", secPlain, newCred.Password)
	}
	if sec.Hash != store.SecretHash(newCred.Password) {
		t.Errorf("db secret hash = %q, want %q", sec.Hash, store.SecretHash(newCred.Password))
	}

	// The NEW value lives under a fresh content-addressed swarm secret; the old
	// fixed-name secret is untouched.
	if string(f.secrets["zo_root_user_password"].Data) != string(oldSecretData) {
		t.Error("the old zo_root_user_password swarm secret must be unchanged after Rotate")
	}
	if _, ok := f.secrets[newCred.SwarmSecretName]; !ok {
		t.Fatalf("the new swarm secret %s must exist after Rotate", newCred.SwarmSecretName)
	}
}

// TestEnsureMaterialized_RecreatesMissingSecretNoRotation verifies the
// self-heal primitive: a missing Swarm secret is recreated from the DB
// ciphertext, an existing one is left untouched, and the credential row is
// never rotated. Uses edge_api_token (plain format, non-rotatable) so the
// recreated payload is byte-identical — exactly what a wiped-Swarm recovery
// needs.
func TestEnsureMaterialized_RecreatesMissingSecretNoRotation(t *testing.T) {
	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()
	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f}

	if _, err := mgr.Bootstrap(context.Background(), BootstrapInput{
		TraefikAdminUser:      "admin",
		OpenObserveAdminEmail: "ops@example.com",
	}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	before, err := s.GetCredential(context.Background(), "edge_api_token")
	if err != nil {
		t.Fatalf("GetCredential edge_api_token: %v", err)
	}
	originalSecret, ok := f.secrets["edge_api_token"]
	if !ok {
		t.Fatal("edge_api_token secret missing after bootstrap")
	}

	// Simulate a wiped swarm: secret gone, DB row intact.
	delete(f.secrets, "edge_api_token")

	created, err := mgr.EnsureMaterialized(context.Background(), "edge_api_token")
	if err != nil {
		t.Fatalf("EnsureMaterialized: %v", err)
	}
	if !created {
		t.Fatal("EnsureMaterialized should report created=true for a missing secret")
	}
	recreated, ok := f.secrets["edge_api_token"]
	if !ok {
		t.Fatal("edge_api_token secret not recreated")
	}
	if !bytes.Equal(recreated.Data, originalSecret.Data) {
		t.Error("recreated secret payload differs from the original (rotated?)")
	}

	// Existing secret must be left untouched: a second call is a no-op.
	again, err := mgr.EnsureMaterialized(context.Background(), "edge_api_token")
	if err != nil {
		t.Fatalf("EnsureMaterialized (existing): %v", err)
	}
	if again {
		t.Error("EnsureMaterialized on an existing secret should report created=false")
	}

	after, err := s.GetCredential(context.Background(), "edge_api_token")
	if err != nil {
		t.Fatalf("GetCredential after: %v", err)
	}
	if !bytes.Equal(before.PasswordCiphertext, after.PasswordCiphertext) {
		t.Error("credential ciphertext changed (rotated) — materialization must never rotate")
	}
}

// TestEnsureMaterialized_HtpasswdRecreatedFromSamePassword verifies the
// htpasswd-format credential (traefik_dashboard → admin_credentials) is
// recreated with the SAME plaintext password (bcrypt is salted, so the hash
// line differs, but it must authenticate the unchanged password).
func TestEnsureMaterialized_HtpasswdRecreatedFromSamePassword(t *testing.T) {
	s, c := newTestDeps(t)
	f := newFakeDocker()
	f.info = goodSwarmInfo()
	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f}

	if _, err := mgr.Bootstrap(context.Background(), BootstrapInput{
		TraefikAdminUser:      "admin",
		OpenObserveAdminEmail: "ops@example.com",
	}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	before, err := s.GetCredential(context.Background(), "traefik_dashboard")
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}
	beforePlain, err := c.Decrypt(before.PasswordCiphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}

	delete(f.secrets, "admin_credentials")

	if _, err := mgr.EnsureMaterialized(context.Background(), "traefik_dashboard"); err != nil {
		t.Fatalf("EnsureMaterialized: %v", err)
	}
	recreated, ok := f.secrets["admin_credentials"]
	if !ok {
		t.Fatal("admin_credentials secret not recreated")
	}
	parts := strings.SplitN(strings.TrimSpace(string(recreated.Data)), ":", 2)
	if len(parts) != 2 || parts[0] != "admin" {
		t.Fatalf("htpasswd line malformed: %q", recreated.Data)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(parts[1]), beforePlain); err != nil {
		t.Errorf("recreated htpasswd line does not authenticate the unchanged password: %v", err)
	}

	after, err := s.GetCredential(context.Background(), "traefik_dashboard")
	if err != nil {
		t.Fatalf("GetCredential after: %v", err)
	}
	if !bytes.Equal(before.PasswordCiphertext, after.PasswordCiphertext) {
		t.Error("credential ciphertext changed (rotated)")
	}
}

// TestEnsureMaterialized_MissingRowIsNoOp verifies a credential absent from the
// DB is skipped (never minted) — materialization only rebuilds missing Swarm
// secrets, it never creates credentials.
func TestEnsureMaterialized_MissingRowIsNoOp(t *testing.T) {
	s, c := newTestDeps(t)
	f := newFakeDocker()
	mgr := &CredentialsManager{Store: s, Cipher: c, Docker: f}

	created, err := mgr.EnsureMaterialized(context.Background(), "sso_cookie_secret")
	if err != nil {
		t.Fatalf("EnsureMaterialized: %v", err)
	}
	if created {
		t.Fatal("missing credential row must be a no-op, not a mint")
	}
	if _, err := s.GetCredential(context.Background(), "sso_cookie_secret"); err != store.ErrCredentialNotFound {
		t.Fatalf("credential row should still be absent, got err=%v", err)
	}
}
