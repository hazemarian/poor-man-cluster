package secrets

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// newHealFixture builds a Local service over a fresh store + cipher.
func newHealFixture(t *testing.T) (*Local, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cipher, err := credentials.Open(filepath.Join(dir, ".encryption_key"))
	if err != nil {
		t.Fatalf("open cipher: %v", err)
	}
	return NewLocal(st, cipher), st
}

func TestHeal_HealthyRowIsOK(t *testing.T) {
	svc, _ := newHealFixture(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "service", "demo", "db_pass", "s3cret-1"); err != nil {
		t.Fatalf("create: %v", err)
	}

	var calls []string
	rep, err := svc.Heal(ctx, HealOptions{Mirror: func(_ context.Context, name, value, hash string) (bool, error) {
		calls = append(calls, name+"="+value+"@"+hash[:8])
		return false, nil // already present
	}})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if rep.OK != 1 || rep.Repaired != 0 || rep.Degraded != 0 || rep.Unrecoverable != 0 {
		t.Fatalf("counts = ok:%d repaired:%d degraded:%d unrec:%d, want 1/0/0/0", rep.OK, rep.Repaired, rep.Degraded, rep.Unrecoverable)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].Status != HealOK {
		t.Fatalf("entries = %+v, want one ok", rep.Entries)
	}
	wantHash := store.SecretHash("s3cret-1")
	if got := rep.Entries[0].Hash; got != wantHash {
		t.Errorf("entry hash = %s, want %s", got, wantHash)
	}
	if len(calls) != 1 || calls[0] != "db_pass=s3cret-1@"+wantHash[:8] {
		t.Errorf("mirror calls = %v, want [db_pass=s3cret-1@%s]", calls, wantHash[:8])
	}
	if !strings.Contains(strings.Join(rep.Entries[0].Actions, "; "), "already present") {
		t.Errorf("actions = %v, want an already-present note", rep.Entries[0].Actions)
	}
}

func TestHeal_CreatesMissingSwarmMirror(t *testing.T) {
	svc, _ := newHealFixture(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "service", "", "app_pass", "value-1"); err != nil {
		t.Fatalf("create: %v", err)
	}

	rep, err := svc.Heal(ctx, HealOptions{Mirror: func(context.Context, string, string, string) (bool, error) {
		return true, nil // created
	}})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if rep.Repaired != 1 || rep.OK != 0 {
		t.Fatalf("counts = ok:%d repaired:%d, want 0/1", rep.OK, rep.Repaired)
	}
	if rep.Entries[0].Status != HealRepaired {
		t.Errorf("status = %s, want repaired", rep.Entries[0].Status)
	}
	if !strings.Contains(strings.Join(rep.Entries[0].Actions, "; "), "swarm mirror created") {
		t.Errorf("actions = %v, want a mirror-created note", rep.Entries[0].Actions)
	}
}

func TestHeal_RepairsStaleHash(t *testing.T) {
	svc, st := newHealFixture(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "service", "", "app_pass", "value-1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulate a row written by older tooling: the ciphertext decrypts, but
	// the stored hash no longer matches the plaintext (translation would then
	// reference a swarm secret that does not exist).
	row, err := st.Secret(ctx, "app_pass")
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	staleHash := "deadbeef" + strings.Repeat("0", 56)
	if err := st.UpdateSecret(ctx, "app_pass", row.Payload, staleHash); err != nil {
		t.Fatalf("corrupt hash: %v", err)
	}

	var mirroredHash string
	rep, err := svc.Heal(ctx, HealOptions{Mirror: func(_ context.Context, _, _, hash string) (bool, error) {
		mirroredHash = hash
		return false, nil
	}})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if rep.Repaired != 1 {
		t.Fatalf("repaired = %d, want 1 (%+v)", rep.Repaired, rep.Entries)
	}
	if !strings.Contains(strings.Join(rep.Entries[0].Actions, "; "), "stale hash repaired") {
		t.Errorf("actions = %v, want a stale-hash repair", rep.Entries[0].Actions)
	}
	real := store.SecretHash("value-1")
	after, err := st.Secret(ctx, "app_pass")
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if after.Hash != real {
		t.Errorf("stored hash = %s, want repaired %s", after.Hash, real)
	}
	if mirroredHash != real {
		t.Errorf("mirrored with hash %s, want the repaired %s", mirroredHash, real)
	}
}

func TestHeal_UnrecoverableCiphertext(t *testing.T) {
	svc, st := newHealFixture(t)
	ctx := context.Background()

	// A row sealed by an OLD encryption key (the key was rotated at some
	// point, so the current cipher can never open it).
	oldCipher, err := credentials.Open(filepath.Join(t.TempDir(), ".encryption_key"))
	if err != nil {
		t.Fatalf("open old cipher: %v", err)
	}
	oldPayload, err := oldCipher.Encrypt([]byte("legacy-value"))
	if err != nil {
		t.Fatalf("encrypt with old key: %v", err)
	}
	if _, err := st.CreateSecret(ctx, "service", "", "legacy_pass", oldPayload, store.SecretHash("legacy-value")); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	var calls int
	rep, err := svc.Heal(ctx, HealOptions{Mirror: func(context.Context, string, string, string) (bool, error) {
		calls++
		return false, nil
	}})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if rep.Unrecoverable != 1 || rep.OK != 0 || rep.Repaired != 0 {
		t.Fatalf("counts = ok:%d repaired:%d unrec:%d, want 0/0/1", rep.OK, rep.Repaired, rep.Unrecoverable)
	}
	e := rep.Entries[0]
	if e.Status != HealUnrecoverable {
		t.Errorf("status = %s, want unrecoverable", e.Status)
	}
	if e.Hash != "" {
		t.Errorf("hash = %q, want empty for an unrecoverable row", e.Hash)
	}
	if calls != 0 {
		t.Errorf("mirror called %d times for an unrecoverable row, want 0", calls)
	}
	detail := strings.Join(e.Actions, "; ")
	if !strings.Contains(detail, "does not decrypt") || !strings.Contains(detail, "pmcluster secret edit legacy_pass") {
		t.Errorf("actions = %v, want a re-supply hint", e.Actions)
	}
}

func TestHeal_DryRunMakesNoChanges(t *testing.T) {
	svc, st := newHealFixture(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "service", "", "app_pass", "value-1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	row, err := st.Secret(ctx, "app_pass")
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	staleHash := "deadbeef" + strings.Repeat("0", 56)
	if err := st.UpdateSecret(ctx, "app_pass", row.Payload, staleHash); err != nil {
		t.Fatalf("corrupt hash: %v", err)
	}

	rep, err := svc.Heal(ctx, HealOptions{
		DryRun: true,
		Mirror: func(context.Context, string, string, string) (bool, error) {
			t.Error("mirror must not be called during a dry run")
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if rep.Repaired != 1 {
		t.Fatalf("repaired = %d, want 1 (would-be)", rep.Repaired)
	}
	detail := strings.Join(rep.Entries[0].Actions, "; ")
	if !strings.Contains(detail, "would be repaired") || !strings.Contains(detail, "would be ensured") {
		t.Errorf("actions = %v, want dry-run wording", rep.Entries[0].Actions)
	}
	after, err := st.Secret(ctx, "app_pass")
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if after.Hash != staleHash {
		t.Errorf("dry run modified the stored hash: %s", after.Hash)
	}
}

func TestHeal_MirrorFailureIsDegraded(t *testing.T) {
	svc, _ := newHealFixture(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "service", "", "app_pass", "value-1"); err != nil {
		t.Fatalf("create: %v", err)
	}

	rep, err := svc.Heal(ctx, HealOptions{Mirror: func(context.Context, string, string, string) (bool, error) {
		return false, errors.New("daemon exploded")
	}})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if rep.Degraded != 1 || rep.OK != 0 || rep.Repaired != 0 {
		t.Fatalf("counts = ok:%d repaired:%d degraded:%d, want 0/0/1", rep.OK, rep.Repaired, rep.Degraded)
	}
	if rep.Entries[0].Status != HealDegraded {
		t.Errorf("status = %s, want degraded", rep.Entries[0].Status)
	}
	if !strings.Contains(strings.Join(rep.Entries[0].Actions, "; "), "daemon exploded") {
		t.Errorf("actions = %v, want the mirror error", rep.Entries[0].Actions)
	}
}

func TestHeal_NoSecretsIsEmptyReport(t *testing.T) {
	svc, _ := newHealFixture(t)
	rep, err := svc.Heal(context.Background(), HealOptions{})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if len(rep.Entries) != 0 || rep.OK != 0 {
		t.Fatalf("report = %+v, want empty", rep)
	}
}
