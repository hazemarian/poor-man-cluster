package clustersnapshot

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func verifyStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "verify.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// applySetting authenticates the store as a mirror of one small snapshot.
func applySetting(t *testing.T, st *store.Store, checked time.Time) {
	t.Helper()
	if err := st.ApplySnapshot(context.Background(), &store.Snapshot{
		Version: 6,
		Tables: map[string]store.TableData{
			"cluster_settings": {Columns: []string{"key", "value"}, Types: []string{"TEXT", "TEXT"},
				Rows: [][]any{{"domain", "example.test"}}},
		},
	}, "nxt-sw-5-m", checked.Unix()-30); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	if err := st.ConfirmMirrorSync(context.Background(), checked.Unix()); err != nil {
		t.Fatalf("ConfirmMirrorSync: %v", err)
	}
}

// TestVerify_EmptyStoreIsAProblem is the check the kit drill used to be, pointed at the
// mirror: a node with no snapshot and no state would promote a leader that serves an empty
// cluster, and the report must say so rather than print a version number.
func TestVerify_EmptyStoreIsAProblem(t *testing.T) {
	st := verifyStore(t)
	rep, err := Verify(context.Background(), st, true, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.OK() {
		t.Fatalf("an empty store verified OK: %+v", rep)
	}
	if !rep.NeverApplied {
		t.Error("NeverApplied is false on a store that has never mirrored")
	}
	if len(rep.Problems) == 0 || !strings.Contains(strings.Join(rep.Problems, " "), "no cluster state") {
		t.Errorf("problems do not name the empty store: %v", rep.Problems)
	}
}

// TestVerify_FreshMirrorNamesVersionAgeAndRows: the three facts an operator needs before a
// promotion — which version, how old, and what came with it.
func TestVerify_FreshMirrorNamesVersionAgeAndRows(t *testing.T) {
	st := verifyStore(t)
	now := time.Now()
	applySetting(t, st, now.Add(-20*time.Second))

	rep, err := Verify(context.Background(), st, true, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("a fresh mirror with a key did not verify: %v", rep.Problems)
	}
	if rep.Leader != "nxt-sw-5-m" || rep.Version != 6 {
		t.Errorf("leader/version = %q/%d, want nxt-sw-5-m/6", rep.Leader, rep.Version)
	}
	if rep.AgeSeconds < 18 || rep.AgeSeconds > 22 {
		t.Errorf("age = %ds, want ~20s from checked_at", rep.AgeSeconds)
	}
	if rep.Tables["cluster_settings"] != 1 {
		t.Errorf("cluster_settings count = %d, want 1", rep.Tables["cluster_settings"])
	}
	if rep.TotalRows < 1 {
		t.Errorf("total rows = %d, want at least the row it carries", rep.TotalRows)
	}
	if !strings.Contains(rep.Human(), "version 6") || !strings.Contains(rep.Human(), "mirror of nxt-sw-5-m") {
		t.Errorf("Human() = %q", rep.Human())
	}
}

// TestVerify_StaleMirrorWarnsAndStillPasses is the operator's ruling applied to the drill:
// an old mirror is a warning, because a promotion on it is accepted. A drill that FAILED on
// it would contradict the adoption path.
func TestVerify_StaleMirrorWarnsAndStillPasses(t *testing.T) {
	st := verifyStore(t)
	now := time.Now()
	applySetting(t, st, now.Add(-10*time.Minute))

	rep, err := Verify(context.Background(), st, true, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("a stale mirror was reported as a problem, not a warning: %v", rep.Problems)
	}
	if len(rep.Warnings) == 0 || !strings.Contains(strings.Join(rep.Warnings, " "), "10m") {
		t.Errorf("warnings do not name the age: %v", rep.Warnings)
	}
}

// TestVerify_MissingKeyWarns: the key is not in the mirror, and this node has none. It must
// be visible (every secret read would fail after a promotion) without failing a report whose
// state is otherwise sound.
func TestVerify_MissingKeyWarns(t *testing.T) {
	st := verifyStore(t)
	applySetting(t, st, time.Now())
	rep, err := Verify(context.Background(), st, true, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.KeyPresent {
		t.Fatal("KeyPresent = false with hasKey=true")
	}

	rep, err = Verify(context.Background(), st, false, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.KeyPresent {
		t.Fatal("KeyPresent = true with hasKey=false")
	}
	if !rep.OK() {
		t.Errorf("a missing key failed the report: %v", rep.Problems)
	}
	if !strings.Contains(strings.Join(rep.Warnings, " "), "no encryption key") {
		t.Errorf("warnings do not name the missing key: %v", rep.Warnings)
	}
}

// TestVerify_StacksWithoutRevisionsWarn: the stack pointer is only useful with the revision
// behind it, and the mirror now carries both, so the drill can say when they disagree.
func TestVerify_StacksWithoutRevisionsWarn(t *testing.T) {
	st := verifyStore(t)
	ctx := context.Background()
	if err := st.ApplySnapshot(ctx, &store.Snapshot{
		Version: 2,
		Tables: map[string]store.TableData{
			"stacks": {Columns: []string{"name", "current_revision", "created_at", "updated_at"},
				Types: []string{"TEXT", "INTEGER", "INTEGER", "INTEGER"},
				Rows:  [][]any{{"web", int64(4), int64(1), int64(1)}}},
		},
	}, "nxt-sw-5-m", time.Now().Unix()); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	rep, err := Verify(ctx, st, true, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !strings.Contains(strings.Join(rep.Warnings, " "), "no stack revision") {
		t.Errorf("warnings do not name the missing revisions: %v", rep.Warnings)
	}
}

func TestVerify_NoStoreIsAnError(t *testing.T) {
	if _, err := Verify(context.Background(), nil, true, time.Now()); err == nil {
		t.Fatal("Verify accepted a nil store")
	}
}
