package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
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
	if newCred.SwarmSecretName != "admin_credentials" {
		t.Errorf("SwarmSecretName = %q, want 'admin_credentials'", newCred.SwarmSecretName)
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

	newSecretData, ok := f.secrets["admin_credentials"]
	if !ok {
		t.Fatal("admin_credentials secret should still exist after re-creation")
	}
	if string(newSecretData.Data) == string(oldSecretData.Data) {
		t.Error("Swarm secret payload should have changed after Rotate")
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

	injectErr := errors.New("secret is in use by a running service")
	f.secretRemoveErr = map[string]error{
		"admin_credentials": injectErr,
	}

	_, err := mgr.Rotate(ctx, "traefik_dashboard")
	if err == nil {
		t.Fatal("Rotate should return error when SecretRemove fails")
	}

	if !strings.Contains(err.Error(), "is the consuming service still using it") {
		t.Errorf("error %q should mention 'is the consuming service still using it'", err.Error())
	}
	if !errors.Is(err, injectErr) {
		t.Errorf("err = %v, want to wrap the injected error", err)
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

	if string(f.secrets["zo_root_user_password"].Data) == string(oldSecretData) {
		t.Error("swarm secret payload should have changed after Rotate")
	}
}
