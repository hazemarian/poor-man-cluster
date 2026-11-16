package controlplane

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// clusterKey is the encryption key the fake cluster published in its key config.
const clusterKey = "0123456789abcdef0123456789abcdef"

// newMirrorStore opens a store and applies a one-setting snapshot, so it looks like a node
// that has been mirroring the leader. checked rounds it out with a confirmation time — the
// column an age may be measured from (applied_at only moves when content changes).
func newMirrorStore(t *testing.T, leader string, version int64, checked time.Time) (*store.Store, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tables := map[string]store.TableData{
		"cluster_settings": {Columns: []string{"key", "value"}, Types: []string{"TEXT", "TEXT"}, Rows: [][]any{{"domain", "example.test"}}},
	}
	if err := st.ApplySnapshot(ctx, &store.Snapshot{
		Version: version, GeneratedAt: checked.Unix() - 60, Tables: tables,
	}, leader, checked.Unix()-30); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	if err := st.ConfirmMirrorSync(ctx, checked.Unix()); err != nil {
		t.Fatalf("ConfirmMirrorSync: %v", err)
	}
	return st, dir
}

// pinClock fixes the adoption clock, and restores it when the test ends: a package-level
// seam left stubbed makes every later test run at a fixed instant.
func pinClock(t *testing.T, at time.Time) {
	t.Helper()
	prev := Now
	Now = func() time.Time { return at }
	t.Cleanup(func() { Now = prev })
}

// writeKeyFile puts a key where a manager expects it: <data dir>/.encryption_key. This is the
// only place a cluster's key lives now (ADR 0006) — there is no config to publish.
func writeKeyFile(t *testing.T, dir string, key []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".encryption_key"), key, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
}

// storeWithSecret writes a secret encrypted under key, so "does this key open this store?"
// has something real to answer. The clause that matters: a secret's ciphertext is only
// meaningful next to the key that produced it.
func storeWithSecret(t *testing.T, dir string, key []byte, value string) *store.Store {
	t.Helper()
	ctx := context.Background()
	writeKeyFile(t, dir, key)
	cipher, err := credentials.Open(filepath.Join(dir, ".encryption_key"))
	if err != nil {
		t.Fatalf("open cipher: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	payload, err := cipher.Encrypt([]byte(value))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := st.CreateSecret(ctx, "cluster", "", "offsite-key", payload, store.SecretHash(value)); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if err := st.ApplySnapshot(ctx, &store.Snapshot{
		Version: 2, GeneratedAt: time.Now().Unix() - 60,
		Tables: map[string]store.TableData{
			"cluster_settings": {Columns: []string{"key", "value"}, Types: []string{"TEXT", "TEXT"}, Rows: [][]any{{"domain", "example.test"}}},
		},
	}, "leader-1", time.Now().Unix()-30); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	if err := st.ConfirmMirrorSync(ctx, time.Now().Unix()); err != nil {
		t.Fatalf("ConfirmMirrorSync: %v", err)
	}
	return st
}

func TestAdoptOnLeadership_StandaloneIsItsOwnSource(t *testing.T) {
	// Standalone: no swarm, so no peer and no mirror, and it has always served. It is also
	// the one case where a nil store is allowed — there is nothing to verify against.
	kit := &Kit{DataDir: t.TempDir(), Standalone: true, Log: zerolog.Nop()}
	adoption, err := kit.AdoptOnLeadership(context.Background(), nil, false)
	if err != nil {
		t.Fatalf("AdoptOnLeadership: %v", err)
	}
	if !adoption.Ready {
		t.Errorf("standalone node is not ready: %+v", adoption)
	}
}

func TestAdoptOnLeadership_FirstLeaderHasNothingToVouchFor(t *testing.T) {
	// The first leader of a cluster (or any node that never ran as a follower in this
	// process) IS the source. Refusing here would make bootstrapping impossible.
	st, _ := newMirrorStore(t, "leader-1", 1, time.Now())
	pinClock(t, time.Now())
	kit := &Kit{DataDir: t.TempDir(), Log: zerolog.Nop()}

	// Clear the mirror: this node has never applied a snapshot.
	if _, err := st.DB().Exec(`DELETE FROM sync_state`); err != nil {
		t.Fatalf("clear sync_state: %v", err)
	}
	adoption, err := kit.AdoptOnLeadership(context.Background(), st, false)
	if err != nil {
		t.Fatalf("AdoptOnLeadership: %v", err)
	}
	if !adoption.Ready || !adoption.Source {
		t.Errorf("adoption = %+v, want ready and named as the source", adoption)
	}
}

// TestAdoptOnLeadership_EmptyStoreRefuses is the ONE hard refusal: never mirrored AND no
// cluster state at all. That is not a stale copy, it is no copy — a leader answering from it
// would report a cluster with no users, no stacks and no settings as the truth.
func TestAdoptOnLeadership_EmptyStoreRefuses(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pinClock(t, time.Now())
	kit := &Kit{DataDir: t.TempDir(), Log: zerolog.Nop()}

	adoption, err := kit.AdoptOnLeadership(context.Background(), st, true)
	if err == nil {
		t.Fatalf("AdoptOnLeadership accepted a node with nothing to serve: %+v", adoption)
	}
	var refusal *RefusalError
	if !errors.As(err, &refusal) {
		t.Errorf("error = %v (%T), want a RefusalError so the caller can name the refusal", err, err)
	}
	if !strings.Contains(err.Error(), "nothing to serve") {
		t.Errorf("refusal does not say why: %v", err)
	}
}

// TestAdoptOnLeadership_NeverMirroredButHoldingStateIsTheSource: a store with data and no
// sync_state is a node that CREATED the cluster (or a standalone box). It adopts itself.
func TestAdoptOnLeadership_NeverMirroredButHoldingStateIsTheSource(t *testing.T) {
	st, _ := newMirrorStore(t, "leader-1", 1, time.Now())
	if _, err := st.DB().Exec(`DELETE FROM sync_state`); err != nil {
		t.Fatalf("clear sync_state: %v", err)
	}
	pinClock(t, time.Now())
	kit := &Kit{DataDir: t.TempDir(), Log: zerolog.Nop()}

	adoption, err := kit.AdoptOnLeadership(context.Background(), st, true)
	if err != nil {
		t.Fatalf("AdoptOnLeadership: %v", err)
	}
	if !adoption.Ready || !adoption.Source {
		t.Errorf("adoption = %+v, want ready and named as the source", adoption)
	}
}

func TestAdoptOnLeadership_FreshMirrorIsReady(t *testing.T) {
	now := time.Now()
	st, _ := newMirrorStore(t, "leader-1", 7, now.Add(-10*time.Second))
	pinClock(t, now)
	kit := &Kit{DataDir: t.TempDir(), Log: zerolog.Nop()}

	adoption, err := kit.AdoptOnLeadership(context.Background(), st, true)
	if err != nil {
		t.Fatalf("AdoptOnLeadership: %v", err)
	}
	if !adoption.Ready || adoption.Leader != "leader-1" || adoption.Version != 7 {
		t.Errorf("adoption = %+v, want ready with leader-1 at version 7", adoption)
	}
	if adoption.Age < 9*time.Second || adoption.Age > 12*time.Second {
		t.Errorf("age = %s, want ~10s from checked_at", adoption.Age)
	}
}

// TestAdoptOnLeadership_StaleMirrorIsServedAndSaysSo is the operator's ruling: a 10-minute
// old mirror is SERVED (a promotion on old data beats no leader), and the cost is made
// visible — the version and the age are logged, and the reason carries them too.
func TestAdoptOnLeadership_StaleMirrorIsServedAndSaysSo(t *testing.T) {
	now := time.Now()
	st, _ := newMirrorStore(t, "wafaa", 4, now.Add(-10*time.Minute))
	pinClock(t, now)
	var buf bytes.Buffer
	kit := &Kit{DataDir: t.TempDir(), Log: zerolog.New(&buf)}

	adoption, err := kit.AdoptOnLeadership(context.Background(), st, true)
	if err != nil {
		t.Fatalf("AdoptOnLeadership refused a stale mirror: %v", err)
	}
	if !adoption.Ready || adoption.Leader != "wafaa" || adoption.Version != 4 {
		t.Errorf("adoption = %+v, want ready on wafaa version 4", adoption)
	}
	if adoption.Age < 9*time.Minute {
		t.Errorf("age = %s, want the age of the mirror it is serving", adoption.Age)
	}
	if !strings.Contains(adoption.Reason, "4") || !strings.Contains(adoption.Reason, "10m") {
		t.Errorf("reason %q does not carry the version and the age", adoption.Reason)
	}
	if line := buf.String(); !strings.Contains(line, "older than three polls") || !strings.Contains(line, `"level":"warn"`) {
		t.Fatalf("a stale adoption was not logged as a warning:\n%s", line)
	}
}

// TestAdoptOnLeadership_ServesTheStoreItAlreadyHas is the point of the whole change,
// measured on the file: adoption must not replace, rename or re-extract data.db. The same
// inode is still there afterwards, with the same row in it.
func TestAdoptOnLeadership_ServesTheStoreItAlreadyHas(t *testing.T) {
	now := time.Now()
	st, dir := newMirrorStore(t, "leader-1", 3, now.Add(-5*time.Second))
	pinClock(t, now)
	dbPath := filepath.Join(dir, "data.db")
	before, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	kit := &Kit{DataDir: dir, Log: zerolog.Nop()}
	adoption, err := kit.AdoptOnLeadership(context.Background(), st, true)
	if err != nil || !adoption.Ready {
		t.Fatalf("AdoptOnLeadership = %+v, %v", adoption, err)
	}

	after, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat after adoption: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("adoption replaced data.db — the mirror is supposed to be served in place, not copied over")
	}
	if got := after.ModTime(); !got.Equal(before.ModTime()) {
		t.Errorf("data.db mtime moved from %s to %s: something rewrote the file", before.ModTime(), got)
	}
	// And the rows are still readable through the SAME handle: no close/reopen happened.
	if v, err := st.Setting(context.Background(), "domain"); err != nil || v != "example.test" {
		t.Errorf("reading through the surviving handle = %q, %v", v, err)
	}
	// No staging directory was left behind by the retired extraction path.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".adopt-") {
			t.Errorf("adoption created a staging directory %q; nothing is staged any more", e.Name())
		}
	}
}

// The key is proven by decrypting what the store holds. Comparing bytes against a published
// config — what this used to do — was a proxy; a wrong key and a right one are indistinguishable
// by name, and only the ciphertext knows.
func TestAdoptOnLeadership_ForeignKeyIsReported(t *testing.T) {
	now := time.Now()
	pinClock(t, now)
	dir := t.TempDir()
	st := storeWithSecret(t, dir, []byte(clusterKey), "s3cr3t-offsite")
	// The node holds a DIFFERENT key: every secret ciphertext it mirrors was written with
	// the other one.
	foreign := []byte("ffffffffffffffffffffffffffffffff")
	writeKeyFile(t, dir, foreign)
	cipher, err := credentials.OpenExisting(filepath.Join(dir, ".encryption_key"))
	if err != nil {
		t.Fatalf("open foreign cipher: %v", err)
	}

	kit := &Kit{DataDir: dir, Cipher: cipher, Log: zerolog.Nop()}
	adoption, err := kit.AdoptOnLeadership(context.Background(), st, true)
	if err != nil {
		t.Fatalf("a foreign key must be reported, not turned into a refusal: %v", err)
	}
	if adoption.KeyOK || !strings.Contains(adoption.KeyProblem, "do not decrypt") {
		t.Errorf("adoption = %+v, want KeyProblem naming the secrets that do not decrypt", adoption)
	}
	if !strings.Contains(adoption.KeyProblem, ".encryption_key") {
		t.Errorf("KeyProblem does not name the file to fix: %q", adoption.KeyProblem)
	}
}

func TestAdoptOnLeadership_MissingKeyFileIsReported(t *testing.T) {
	now := time.Now()
	pinClock(t, now)
	dir := t.TempDir()
	st := storeWithSecret(t, dir, []byte(clusterKey), "s3cr3t-offsite")
	if err := os.Remove(filepath.Join(dir, ".encryption_key")); err != nil {
		t.Fatalf("remove key: %v", err)
	}

	kit := &Kit{DataDir: dir, Log: zerolog.Nop()}
	adoption, err := kit.AdoptOnLeadership(context.Background(), st, true)
	if err != nil {
		t.Fatalf("a missing key must be reported, not turned into a refusal: %v", err)
	}
	if adoption.KeyOK || !strings.Contains(adoption.KeyProblem, "no encryption key at") {
		t.Errorf("adoption = %+v, want KeyProblem about the missing key file", adoption)
	}
	if !strings.Contains(adoption.KeyProblem, "copy it") {
		t.Errorf("KeyProblem does not tell the operator what to do: %q", adoption.KeyProblem)
	}
}

func TestAdoptOnLeadership_MatchingKeyIsReady(t *testing.T) {
	now := time.Now()
	pinClock(t, now)
	dir := t.TempDir()
	st := storeWithSecret(t, dir, []byte(clusterKey), "s3cr3t-offsite")
	cipher, err := credentials.OpenExisting(filepath.Join(dir, ".encryption_key"))
	if err != nil {
		t.Fatalf("open cipher: %v", err)
	}

	kit := &Kit{DataDir: dir, Cipher: cipher, Log: zerolog.Nop()}
	adoption, err := kit.AdoptOnLeadership(context.Background(), st, true)
	if err != nil || !adoption.Ready || !adoption.KeyOK {
		t.Fatalf("AdoptOnLeadership = %+v, %v", adoption, err)
	}
}

func TestAdoptOnLeadership_StoreWithoutSecretsHasNothingToDisagreeWith(t *testing.T) {
	// A cluster with no secrets yet has no ciphertext to prove anything against. Reporting a
	// key problem here would make every bootstrap look broken.
	now := time.Now()
	st, dir := newMirrorStore(t, "leader-1", 2, now.Add(-5*time.Second))
	pinClock(t, now)
	writeKeyFile(t, dir, []byte(clusterKey))
	cipher, err := credentials.OpenExisting(filepath.Join(dir, ".encryption_key"))
	if err != nil {
		t.Fatalf("open cipher: %v", err)
	}

	kit := &Kit{DataDir: dir, Cipher: cipher, Log: zerolog.Nop()}
	adoption, err := kit.AdoptOnLeadership(context.Background(), st, true)
	if err != nil || !adoption.Ready || !adoption.KeyOK {
		t.Fatalf("AdoptOnLeadership = %+v, %v", adoption, err)
	}
}

func TestAdoptOnLeadership_NoStoreIsAnError(t *testing.T) {
	kit := &Kit{DataDir: t.TempDir(), Log: zerolog.Nop()}
	if _, err := kit.AdoptOnLeadership(context.Background(), nil, true); err == nil {
		t.Fatal("AdoptOnLeadership accepted a nil store")
	}
}

// TestAdoptOnLeadership_ClockBehindIsNotNegativeAge: a backwards clock must not read as
// "fresh in the future" and must not flip a healthy mirror into a refusal.
func TestAdoptOnLeadership_ClockBehindIsNotNegativeAge(t *testing.T) {
	now := time.Now()
	st, _ := newMirrorStore(t, "leader-1", 2, now.Add(30*time.Second))
	pinClock(t, now)
	kit := &Kit{DataDir: t.TempDir(), Log: zerolog.Nop()}

	adoption, err := kit.AdoptOnLeadership(context.Background(), st, true)
	if err != nil || !adoption.Ready {
		t.Fatalf("AdoptOnLeadership = %+v, %v", adoption, err)
	}
	if adoption.Age < 0 {
		t.Errorf("age = %s, want it clamped at zero", adoption.Age)
	}
}
