package cluster

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// TestMigrateObservabilityNaming covers the OpenObserve-era names that outlive
// the Parseable swap. Two properties matter more than the rename itself:
//
//   - the password is CARRIED OVER, never rotated. The running Parseable, the
//     collector's exporter, the injection middleware, the sign-in bridge and the
//     operator's live browser session all depend on that exact value.
//   - running it twice changes nothing, so it is safe on every `cluster update`.
func TestMigrateObservabilityNaming(t *testing.T) {
	s, cipher := newTestDeps(t)
	f := newFakeDocker()
	ctx := context.Background()

	plaintext := "correct-horse-battery-staple"
	ct, err := cipher.Encrypt([]byte(plaintext))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := s.InsertCredential(ctx, &store.ManagedCredential{
		Name:               "openobserve_admin",
		Kind:               "openobserve",
		Username:           "admin@example.com",
		PasswordCiphertext: ct,
		SwarmSecretName:    "zo_root_user_password",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	for k, v := range map[string]string{
		"oo_admin_email":           "admin@example.com",
		"oo_logs_retention_days":   "30",
		"oo_traces_retention_days": "10",
	} {
		if err := s.SetSetting(ctx, k, v); err != nil {
			t.Fatalf("seed setting %s: %v", k, err)
		}
	}
	// A stale secret: the unversioned base plus a rotated variant, which is how
	// three zo_root_user_password* secrets accumulated on the live node.
	f.secrets["zo_root_user_password"] = secretSpec("zo_root_user_password")
	f.secrets["zo_root_user_password_0eef977e"] = secretSpec("zo_root_user_password_0eef977e")
	f.secrets["unrelated_secret"] = secretSpec("unrelated_secret")

	rep, err := MigrateObservabilityNaming(ctx, s, f)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !rep.CredentialRenamed {
		t.Error("credential should have been renamed")
	}

	// ── the credential moved, intact ──
	got, err := s.Credential(ctx, observabilityCredentialName)
	if err != nil {
		t.Fatalf("read migrated credential: %v", err)
	}
	if !bytes.Equal(got.PasswordCiphertext, ct) {
		t.Error("ciphertext changed — the migration must never rotate the password")
	}
	back, err := cipher.Decrypt(got.PasswordCiphertext)
	if err != nil || string(back) != plaintext {
		t.Errorf("decrypted password = %q (err %v), want the original", back, err)
	}
	if got.Username != "admin@example.com" {
		t.Errorf("username = %q, want preserved", got.Username)
	}
	if got.Kind != string(KindObservability) {
		t.Errorf("kind = %q, want %q", got.Kind, KindObservability)
	}
	if got.SwarmSecretName != observabilitySecretBase {
		t.Errorf("swarm secret = %q, want %q", got.SwarmSecretName, observabilitySecretBase)
	}
	if _, err := s.Credential(ctx, "openobserve_admin"); !errors.Is(err, store.ErrCredentialNotFound) {
		t.Error("the legacy credential row must be gone")
	}

	// ── settings moved, values intact ──
	for newKey, want := range map[string]string{
		"observability_admin_email":           "admin@example.com",
		"observability_logs_retention_days":   "30",
		"observability_traces_retention_days": "10",
	} {
		v, err := s.Setting(ctx, newKey)
		if err != nil || v != want {
			t.Errorf("setting %s = %q (err %v), want %q", newKey, v, err, want)
		}
	}
	for _, oldKey := range []string{"oo_admin_email", "oo_logs_retention_days", "oo_traces_retention_days"} {
		if _, err := s.Setting(ctx, oldKey); !errors.Is(err, store.ErrSettingNotFound) {
			t.Errorf("legacy setting %s must be deleted", oldKey)
		}
	}

	// ── only the OO-named secrets went ──
	if _, ok := f.secrets["zo_root_user_password"]; ok {
		t.Error("unversioned legacy secret must be removed")
	}
	if _, ok := f.secrets["zo_root_user_password_0eef977e"]; ok {
		t.Error("rotated legacy variant must be removed too")
	}
	if _, ok := f.secrets["unrelated_secret"]; !ok {
		t.Error("an unrelated secret must be left alone")
	}

	// ── idempotent ──
	rep2, err := MigrateObservabilityNaming(ctx, s, f)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if rep2.CredentialRenamed || len(rep2.SettingsRenamed) != 0 || len(rep2.SecretsRemoved) != 0 {
		t.Errorf("second run must be a no-op, got %+v", rep2)
	}
}

func secretSpec(name string) runtime.SecretSpec {
	return runtime.SecretSpec{Name: name}
}
