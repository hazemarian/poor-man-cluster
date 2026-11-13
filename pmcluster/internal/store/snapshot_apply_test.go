package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// snap builds a snapshot with the given rows, for tests that care about one table.
func snap(version int64, settings []SnapshotSetting, secrets []SnapshotSecret, users []SnapshotUser) *Snapshot {
	return &Snapshot{Version: version, GeneratedAt: 1_700_000_000, Settings: settings, Secrets: secrets, Users: users}
}

func set(k, v string) SnapshotSetting { return SnapshotSetting{Key: k, Value: v} }

// sec builds one secret row. The id is explicit because secrets.id is a primary key:
// two rows sharing it collide even when their names differ, which is the leader's
// problem to avoid and the mirror's to reproduce faithfully.
func sec(id int64, name string) SnapshotSecret {
	return SnapshotSecret{ID: id, Scope: "cluster", Name: name, Payload: []byte("ciphertext"), Hash: "hash", CreatedAt: 1}
}

func usr(id int64, name, tokenID string) SnapshotUser {
	return SnapshotUser{ID: id, Name: name, TokenID: tokenID, TokenHash: "argon2", Role: "admin", CreatedAt: 1}
}

// TestApplySnapshotWritesTheRowsAndStampsTheirArrival: the write half of the mirror.
// Every row the apply touches carries mirrored_at = the apply's own clock (never the
// leader's), and sync_state records the pair this node can now send back as an ETag.
func TestApplySnapshotWritesTheRowsAndStampsTheirArrival(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.ApplySnapshot(ctx, snap(7,
		[]SnapshotSetting{set("domain", "example.test")},
		[]SnapshotSecret{sec(1, "sso_client_secret")},
		[]SnapshotUser{usr(1, "edge", "ab12"), usr(2, "ci", "")},
	), "nxt-sw-5-m", 1_700_000_500); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	if got := st.SettingDefault(ctx, "domain", ""); got != "example.test" {
		t.Errorf("setting = %q, want the mirrored value", got)
	}
	if _, err := st.Secret(ctx, "sso_client_secret"); err != nil {
		t.Errorf("mirrored secret missing: %v", err)
	}
	if n := countIn(t, st.DB(), "users"); n != 2 {
		t.Errorf("users = %d, want 2", n)
	}
	// mirrored_at is per row, on cluster_settings, secrets and users alike.
	for _, q := range []string{
		`SELECT COUNT(*) FROM cluster_settings WHERE mirrored_at = 1700000500`,
		`SELECT COUNT(*) FROM secrets          WHERE mirrored_at = 1700000500`,
		`SELECT COUNT(*) FROM users            WHERE mirrored_at = 1700000500`,
	} {
		var n int
		if err := st.DB().QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		if n != 1 && n != 2 {
			t.Errorf("%q = %d rows stamped with the apply's clock, want every row", q, n)
		}
	}

	stt, err := st.SyncState(ctx)
	if err != nil {
		t.Fatalf("SyncState: %v", err)
	}
	want := SyncState{Leader: "nxt-sw-5-m", Version: 7, GeneratedAt: 1_700_000_000, AppliedAt: 1_700_000_500, CheckedAt: 1_700_000_500}
	if *stt != want {
		t.Errorf("sync state = %+v, want %+v", *stt, want)
	}
}

// TestApplySnapshotDeletesAbsentRows is the rule an upsert-only mirror gets wrong: a
// row deleted on the leader must disappear here, and nothing else has to be said for
// that to happen. Remove the delete-absent step and this fails with the stale row
// still present.
func TestApplySnapshotDeletesAbsentRows(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.ApplySnapshot(ctx, snap(1,
		[]SnapshotSetting{set("keep", "1"), set("gone", "2")},
		[]SnapshotSecret{sec(1, "keep_secret"), sec(2, "gone_secret")},
		[]SnapshotUser{usr(1, "edge", "ab12"), usr(2, "ci", "cd34")},
	), "nxt-sw-5-m", 100); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// The leader deleted one row of each table and bumped the version.
	if err := st.ApplySnapshot(ctx, snap(2,
		[]SnapshotSetting{set("keep", "1")},
		[]SnapshotSecret{sec(1, "keep_secret")},
		[]SnapshotUser{usr(1, "edge", "ab12")},
	), "nxt-sw-5-m", 200); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	if _, err := st.Setting(ctx, "gone"); !errors.Is(err, ErrSettingNotFound) {
		t.Errorf("deleted setting survived the mirror: %v", err)
	}
	if _, err := st.Secret(ctx, "gone_secret"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("deleted secret survived the mirror: %v", err)
	}
	if n := countIn(t, st.DB(), "users"); n != 1 {
		t.Errorf("users = %d after the leader revoked one, want 1", n)
	}
	if got := st.SettingDefault(ctx, "keep", ""); got != "1" {
		t.Errorf("the row that stayed was lost: %q", got)
	}
}

// TestApplySnapshotEmptyTableClearsIt: the endpoint always sends every table, so an
// empty array is the leader saying "I have none" — and the mirror must delete them
// all. This is the delete-all branch that a `len(keys)==0` guard would otherwise skip.
func TestApplySnapshotEmptyTableClearsIt(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.ApplySnapshot(ctx, snap(1, []SnapshotSetting{set("a", "1"), set("b", "2")}, nil, nil), "nxt-sw-5-m", 100); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := st.ApplySnapshot(ctx, snap(2, []SnapshotSetting{}, []SnapshotSecret{}, []SnapshotUser{}), "nxt-sw-5-m", 200); err != nil {
		t.Fatalf("empty apply: %v", err)
	}
	if n := countIn(t, st.DB(), "cluster_settings"); n != 0 {
		t.Errorf("cluster_settings = %d after an empty snapshot, want 0", n)
	}
}

// TestApplySnapshotIsOneTransaction: a failure part-way through leaves NOTHING applied
// — not the settings that were already upserted, and not the new version. Half an
// apply is worse than none: it would look like a successful sync of a state the leader
// never had.
func TestApplySnapshotIsOneTransaction(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.ApplySnapshot(ctx, snap(1, []SnapshotSetting{set("domain", "old")}, nil, nil), "nxt-sw-5-m", 100); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// The secret's scope violates the table's CHECK constraint, so this apply fails
	// AFTER the settings upsert has run inside the transaction.
	bad := sec(1, "broken")
	bad.Scope = "not-a-scope"
	err := st.ApplySnapshot(ctx, snap(2, []SnapshotSetting{set("domain", "new")}, []SnapshotSecret{bad}, nil), "nxt-sw-5-m", 200)
	if err == nil {
		t.Fatal("an apply with an invalid row succeeded")
	}
	if got := st.SettingDefault(ctx, "domain", ""); got != "old" {
		t.Errorf("setting = %q after a failed apply, want the pre-apply value — the transaction rolled back nothing", got)
	}
	stt, serr := st.SyncState(ctx)
	if serr != nil {
		t.Fatalf("SyncState: %v", serr)
	}
	if stt.Version != 1 || stt.AppliedAt != 100 {
		t.Errorf("sync state = %+v after a failed apply, want the previous version 1 @100", *stt)
	}
	if _, serr := st.Secret(ctx, "broken"); !errors.Is(serr, ErrSecretNotFound) {
		t.Errorf("a row from the failed apply is present: %v", serr)
	}
}

// TestApplySnapshotRefusesAnAnonymousLeader: without the leader the recorded state
// cannot produce the ETag the next poll sends, so this node would re-download the same
// version forever while looking synced. Refusing is the honest answer.
func TestApplySnapshotRefusesAnAnonymousLeader(t *testing.T) {
	st := openTestStore(t)
	if err := st.ApplySnapshot(context.Background(), snap(1, nil, nil, nil), "  ", 100); err == nil {
		t.Fatal("an apply with no leader was accepted")
	}
	if _, err := st.SyncState(context.Background()); !errors.Is(err, ErrNeverSynced) {
		t.Errorf("a refused apply recorded a sync state: %v", err)
	}
	if err := st.ApplySnapshot(context.Background(), nil, "nxt-sw-5-m", 100); err == nil {
		t.Fatal("a nil snapshot was accepted")
	}
}

// TestSyncStateIsNeverSyncedOnAFreshStore: the absence of a row is a real state, and
// it is what lets a read surface say "no age yet" instead of inventing one.
func TestSyncStateIsNeverSyncedOnAFreshStore(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.SyncState(context.Background()); !errors.Is(err, ErrNeverSynced) {
		t.Fatalf("SyncState on a fresh store = %v, want ErrNeverSynced", err)
	}
}

// TestApplySnapshotReplacesUsersDespiteIdDrift pins WHY the users table is replaced
// rather than upserted. This node holds an "edge" row under a different id than the
// leader's; an ON CONFLICT(id) upsert would fail the whole apply on users.name's
// UNIQUE index and stall the mirror permanently.
func TestApplySnapshotReplacesUsersDespiteIdDrift(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	// Two local rows first, so "edge" lands under id 2 — the drift the replace exists
	// for. (With "edge" at id 1 the case would not be exercised at all.)
	if _, err := st.CreateUser(ctx, "aaa-local", "aa11", "local-hash"); err != nil {
		t.Fatalf("seed local user: %v", err)
	}
	if _, err := st.CreateUser(ctx, "edge", "zz99", "local-hash"); err != nil {
		t.Fatalf("seed local user: %v", err)
	}
	local, err := st.UserByName(ctx, "edge")
	if err != nil {
		t.Fatalf("UserByName: %v", err)
	}
	if local.ID == 1 {
		t.Fatalf("the seeded edge row has id 1; the drift case is not exercised")
	}

	// The leader says edge is id 1 and "ci" is id 2 — both of which this node already
	// uses for other names.
	rows := []SnapshotUser{usr(1, "edge", "ab12"), usr(2, "ci", "cd34")}
	if err := st.ApplySnapshot(ctx, snap(1, nil, nil, rows), "nxt-sw-5-m", 100); err != nil {
		t.Fatalf("ApplySnapshot over a name conflict: %v", err)
	}
	got, err := st.UserByName(ctx, "edge")
	if err != nil {
		t.Fatalf("UserByName after apply: %v", err)
	}
	if got.ID != 1 {
		t.Errorf("edge id = %d after the mirror, want the leader's 1", got.ID)
	}
	if n := countIn(t, st.DB(), "users"); n != len(rows) {
		t.Errorf("users = %d, want exactly the leader's %d rows", n, len(rows))
	}
}

// TestApplySnapshotInsertsLegacyUsersAsNullTokenID: token_id is UNIQUE when set, so two
// legacy rows inserted as "" collide and fail the whole apply (and, before that, the
// mirror stalls forever). NULL is the only faithful spelling of "no token id".
func TestApplySnapshotInsertsLegacyUsersAsNullTokenID(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.ApplySnapshot(ctx, snap(1, nil, nil,
		[]SnapshotUser{usr(1, "old-a", ""), usr(2, "old-b", "")}), "nxt-sw-5-m", 100); err != nil {
		t.Fatalf("ApplySnapshot with two legacy users: %v", err)
	}
	var notNull int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM users WHERE token_id IS NOT NULL`).Scan(&notNull); err != nil {
		t.Fatalf("count: %v", err)
	}
	if notNull != 0 {
		t.Errorf("%d legacy users carry a non-NULL token_id, want 0", notNull)
	}
}

// TestApplySnapshotAcceptsAVersionThatMovesBackwards: the version identifies content,
// not time. A leader acting on an adopted store can answer a number it has used before,
// and the follower must apply it — refusing would silently pin the mirror to state the
// leader has moved off.
func TestApplySnapshotAcceptsAVersionThatMovesBackwards(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.ApplySnapshot(ctx, snap(9, []SnapshotSetting{set("domain", "nine")}, nil, nil), "nxt-sw-5-m", 100); err != nil {
		t.Fatalf("apply v9: %v", err)
	}
	if err := st.ApplySnapshot(ctx, snap(3, []SnapshotSetting{set("domain", "three")}, nil, nil), "nxt-sw-5-m", 200); err != nil {
		t.Fatalf("apply v3: %v", err)
	}
	if got := st.SettingDefault(ctx, "domain", ""); got != "three" {
		t.Errorf("setting = %q, want the later-applied content", got)
	}
	stt, err := st.SyncState(ctx)
	if err != nil {
		t.Fatalf("SyncState: %v", err)
	}
	if stt.Version != 3 {
		t.Errorf("sync version = %d, want 3", stt.Version)
	}
	if strings.Compare(stt.Leader, "nxt-sw-5-m") != 0 {
		t.Errorf("sync leader = %q", stt.Leader)
	}
}
