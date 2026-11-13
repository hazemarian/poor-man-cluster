package store

import (
	"context"
	"errors"
	"testing"
)

// version reads the counter, failing the test on error.
func version(t *testing.T, st *Store) int64 {
	t.Helper()
	v, _, err := st.ClusterVersion(context.Background())
	if err != nil {
		t.Fatalf("ClusterVersion: %v", err)
	}
	return v
}

// TestClusterVersionIsSeeded: every store this binary opens answers a version, so
// the snapshot endpoint's first answer is a 200 rather than an error about a row
// nobody created. Remove the seed from migration 0030 and this fails.
func TestClusterVersionIsSeeded(t *testing.T) {
	st := openTestStore(t)
	v, generatedAt, err := st.ClusterVersion(context.Background())
	if err != nil {
		t.Fatalf("ClusterVersion: %v", err)
	}
	if v < 1 {
		t.Errorf("version = %d right after migrations, want at least 1", v)
	}
	if generatedAt <= 0 {
		t.Errorf("generated_at = %d, want a stamped unix second", generatedAt)
	}
}

// TestSetSettingBumpsOnlyOnChange is the rule that keeps the ETag useful in both
// directions: a real change must move the version (or a follower never sees it) and
// rewriting the SAME value must not (or the reconcile loop's per-pass rewrites turn
// every poll into a 200 carrying a full body).
func TestSetSettingBumpsOnlyOnChange(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	before := version(t, st)
	if err := st.SetSetting(ctx, "domain", "example.test"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	afterWrite := version(t, st)
	if afterWrite <= before {
		t.Fatalf("version = %d after a real setting write, want > %d", afterWrite, before)
	}

	// Same value again, twice: neither may bump.
	for i := 0; i < 2; i++ {
		if err := st.SetSetting(ctx, "domain", "example.test"); err != nil {
			t.Fatalf("SetSetting (repeat %d): %v", i, err)
		}
	}
	if got := version(t, st); got != afterWrite {
		t.Errorf("version = %d after rewriting the same value, want %d — a no-op write must not invalidate every follower's ETag", got, afterWrite)
	}

	// A different value bumps again.
	if err := st.SetSetting(ctx, "domain", "other.test"); err != nil {
		t.Fatalf("SetSetting (change): %v", err)
	}
	if got := version(t, st); got <= afterWrite {
		t.Errorf("version = %d after a value change, want > %d", got, afterWrite)
	}
}

// TestDeleteSettingBumpsOnlyWhenARowWent: the naming migration deletes old keys
// unconditionally on every start, so an absent key must stay a silent no-op and must
// not publish a new version at every daemon boot.
func TestDeleteSettingBumpsOnlyWhenARowWent(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	before := version(t, st)
	if err := st.DeleteSetting(ctx, "never-existed"); err != nil {
		t.Fatalf("DeleteSetting on an absent key must not error: %v", err)
	}
	if got := version(t, st); got != before {
		t.Errorf("version = %d after deleting an absent key, want %d", got, before)
	}

	if err := st.SetSetting(ctx, "domain", "example.test"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	withRow := version(t, st)
	if err := st.DeleteSetting(ctx, "domain"); err != nil {
		t.Fatalf("DeleteSetting: %v", err)
	}
	if got := version(t, st); got <= withRow {
		t.Errorf("version = %d after a delete that removed a row, want > %d — the delete is how a follower learns to drop it", got, withRow)
	}
}

// TestSecretWritesBump: every secret write is a content change (create, rotate,
// retag, delete), and a retag to the values the row already carries is not.
func TestSecretWritesBump(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	payload := []byte("ciphertext")

	before := version(t, st)
	if _, err := st.CreateSecret(ctx, "cluster", "", "sso_client_secret", payload, SecretHash("s3cret")); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	created := version(t, st)
	if created <= before {
		t.Fatalf("version = %d after CreateSecret, want > %d", created, before)
	}

	if err := st.UpdateSecret(ctx, "sso_client_secret", []byte("rotated"), SecretHash("rotated")); err != nil {
		t.Fatalf("UpdateSecret: %v", err)
	}
	rotated := version(t, st)
	if rotated <= created {
		t.Errorf("version = %d after a rotation, want > %d", rotated, created)
	}

	// Retagging to the values already carried is a no-op.
	if err := st.UpdateSecretStack(ctx, "sso_client_secret", "cluster", ""); err != nil {
		t.Fatalf("UpdateSecretStack (same): %v", err)
	}
	if got := version(t, st); got != rotated {
		t.Errorf("version = %d after an identical retag, want %d", got, rotated)
	}

	if err := st.UpdateSecretStack(ctx, "sso_client_secret", "service", "billing"); err != nil {
		t.Fatalf("UpdateSecretStack (change): %v", err)
	}
	retagged := version(t, st)
	if retagged <= rotated {
		t.Errorf("version = %d after a real retag, want > %d", retagged, rotated)
	}

	if err := st.DeleteSecret(ctx, "sso_client_secret"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if got := version(t, st); got <= retagged {
		t.Errorf("version = %d after DeleteSecret, want > %d", got, retagged)
	}
}

// TestUserWritesBumpExceptLastUsed pins both halves of the users rule: minting and
// revoking a token are cluster state, and a token's last use is not — bumping on
// every authenticated request would keep every follower permanently re-fetching.
func TestUserWritesBumpExceptLastUsed(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	before := version(t, st)
	id, err := st.CreateUser(ctx, "edge", "ab12", "argon2-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	created := version(t, st)
	if created <= before {
		t.Fatalf("version = %d after CreateUser, want > %d", created, before)
	}

	if err := st.TouchUser(ctx, id); err != nil {
		t.Fatalf("TouchUser: %v", err)
	}
	if got := version(t, st); got != created {
		t.Errorf("version = %d after TouchUser, want %d — last_used_at is not in the snapshot", got, created)
	}

	if err := st.DeleteUser(ctx, id); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if got := version(t, st); got <= created {
		t.Errorf("version = %d after DeleteUser, want > %d", got, created)
	}
}

// TestDeleteStackBumpsWhenItTakesSecretsWithIt: a stack owns its service-scope
// secrets, and `secrets` is a mirrored table — so tearing a stack down without a bump
// leaves every follower holding secret material the leader no longer has. A missing
// stack that owns nothing must stay a silent no-op.
func TestDeleteStackBumpsWhenItTakesSecretsWithIt(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	before := version(t, st)
	if err := st.DeleteStack(ctx, "never-existed"); !errors.Is(err, ErrStackNotFound) {
		t.Fatalf("DeleteStack on a missing stack = %v, want ErrStackNotFound", err)
	}
	if got := version(t, st); got != before {
		t.Errorf("version = %d after deleting a stack that owned nothing, want %d", got, before)
	}

	if _, err := st.CreateSecret(ctx, "service", "billing", "db_password", []byte("ciphertext"), "hash"); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	withSecret := version(t, st)
	if err := st.DeleteStack(ctx, "billing"); !errors.Is(err, ErrStackNotFound) {
		// No stacks row was ever created, and that is the point: the owned secret is
		// still removed, and the not-found answer does not undo it.
		t.Fatalf("DeleteStack = %v, want ErrStackNotFound", err)
	}
	if got := version(t, st); got <= withSecret {
		t.Errorf("version = %d after DeleteStack removed an owned secret, want > %d — a follower would keep it forever", got, withSecret)
	}
	if _, err := st.Secret(ctx, "db_password"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("the stack's secret survived the delete: %v", err)
	}
}

// TestSnapshotReadsTheVersionAndTheRowsTogether: the snapshot's version must be the
// counter that describes exactly the rows in it. Two reads at the same version must
// carry the same rows, in the same order.
func TestSnapshotReadsTheVersionAndTheRowsTogether(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for _, key := range []string{"zeta", "alpha", "mike"} {
		if err := st.SetSetting(ctx, key, "v-"+key); err != nil {
			t.Fatalf("SetSetting %s: %v", key, err)
		}
	}

	snap, err := st.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got, want := snap.Version, version(t, st); got != want {
		t.Errorf("snapshot version = %d, want the store's %d", got, want)
	}
	if len(snap.Settings) != 3 {
		t.Fatalf("snapshot carries %d settings, want 3", len(snap.Settings))
	}
	// Ordered by key, so the same version always renders the same bytes.
	wantOrder := []string{"alpha", "mike", "zeta"}
	for i, key := range wantOrder {
		if snap.Settings[i].Key != key {
			t.Errorf("setting[%d] = %q, want %q (snapshot order must be deterministic)", i, snap.Settings[i].Key, key)
		}
	}
	if snap.Secrets == nil || snap.Users == nil {
		t.Error("empty tables must be non-nil (they marshal to [], not null)")
	}

	// A delete is visible as an absence, and it is what a follower deletes on.
	if err := st.DeleteSetting(ctx, "mike"); err != nil {
		t.Fatalf("DeleteSetting: %v", err)
	}
	after, err := st.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if after.Version <= snap.Version {
		t.Errorf("version = %d after a delete, want > %d", after.Version, snap.Version)
	}
	for _, row := range after.Settings {
		if row.Key == "mike" {
			t.Error("the deleted setting is still in the snapshot")
		}
	}
}
