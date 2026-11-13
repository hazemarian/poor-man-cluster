package doctor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// mirrorStore is a follower's store with one applied snapshot.
func mirrorStore(t *testing.T, appliedAt int64) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ApplySnapshot(context.Background(), &store.Snapshot{
		Version:     7,
		GeneratedAt: appliedAt,
		Settings:    []store.SnapshotSetting{{Key: "domain", Value: "example.test"}},
		Secrets:     []store.SnapshotSecret{},
		Users:       []store.SnapshotUser{},
	}, "nxt-sw-5-m", appliedAt); err != nil {
		t.Fatalf("apply snapshot: %v", err)
	}
	return st
}

// nodeInputWithMirror is the follower's report input: a daemon that answers, a volume root
// that exists, and the mirror under test. Docker is nil (no runner on a CI box), which each
// check reports as a skip instead of guessing.
func nodeInputWithMirror(t *testing.T, st *store.Store, now time.Time) NodeInput {
	t.Helper()
	return NodeInput{
		Hostname:   "nxt-sw-1-m",
		ListenAddr: "127.0.0.1:9090",
		VolumeRoot: t.TempDir(),
		Role:       "manager",
		Store:      st,
		Now:        now,
		Health:     func(context.Context, string) (int, error) { return 200, nil },
	}
}

// TestRunNodeLocalAnswersTheMirrorWithItsAge is the slice-3 requirement: a follower no
// longer prints "not answered here" for the settings it mirrors. It answers, and the
// answer carries the leader and the age.
func TestRunNodeLocalAnswersTheMirrorWithItsAge(t *testing.T) {
	applied := int64(1_700_000_000)
	st := mirrorStore(t, applied)
	rep := RunNodeLocal(context.Background(), nodeInputWithMirror(t, st, time.Unix(applied+42, 0)))

	m, ok := find(rep, "cluster mirror")
	if !ok {
		t.Fatal("the follower report has no cluster mirror check")
	}
	if m.Status != StatusOK {
		t.Fatalf("mirror check = %+v, want OK (the mirror is 42s old)", m)
	}
	for _, want := range []string{"nxt-sw-5-m", "age_seconds=42", "version 7"} {
		if !strings.Contains(m.Detail, want) {
			t.Errorf("the mirror answer must name %q: %s", want, m.Detail)
		}
	}
}

// TestRunNodeLocalWarnsWhenTheMirrorIsStale: a mirror nobody has refreshed since the leader
// died is a FINDING, not a green tick. This is the "killing the leader leaves followers
// serving their mirror, labelled stale" half of the acceptance.
func TestRunNodeLocalWarnsWhenTheMirrorIsStale(t *testing.T) {
	applied := int64(1_700_000_000)
	st := mirrorStore(t, applied)
	// Ten minutes: five poll intervals, well past the three-interval threshold.
	rep := RunNodeLocal(context.Background(), nodeInputWithMirror(t, st, time.Unix(applied+600, 0)))

	m, ok := find(rep, "cluster mirror")
	if !ok {
		t.Fatal("the follower report has no cluster mirror check")
	}
	if m.Status != StatusWarn {
		t.Fatalf("mirror check = %+v, want WARN for a 10-minute-old mirror", m)
	}
	if !strings.Contains(m.Detail, "STALE") || !strings.Contains(m.Detail, "nxt-sw-5-m") {
		t.Errorf("a stale mirror must say so and name the leader it came from: %s", m.Detail)
	}
	// The values are still described, not hidden: stale is not blank.
	if !strings.Contains(m.Detail, "age_seconds=600") {
		t.Errorf("the stale answer must still carry its age: %s", m.Detail)
	}
}

// TestRunNodeLocalWarnsWhenTheMirrorHasNeverSynced: a follower whose mirror has never
// reached the leader is a node that will answer from an empty table — worth a WARN naming
// the poll, not a silent skip.
func TestRunNodeLocalWarnsWhenTheMirrorHasNeverSynced(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "never.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rep := RunNodeLocal(context.Background(), nodeInputWithMirror(t, st, time.Unix(1_700_000_000, 0)))
	m, ok := find(rep, "cluster mirror")
	if !ok {
		t.Fatal("the follower report has no cluster mirror check")
	}
	if m.Status != StatusWarn {
		t.Fatalf("mirror check = %+v, want WARN when no snapshot has been applied", m)
	}
	if !strings.Contains(m.Detail, "60 s") {
		t.Errorf("the answer must say when the mirror normally fills: %s", m.Detail)
	}
}

// TestRunNodeLocalWorkerHasNoMirror: a worker has no store, and the check says so. Opening
// one there would seed an empty database whose answers look plausible.
func TestRunNodeLocalWorkerHasNoMirror(t *testing.T) {
	in := NodeInput{
		Hostname:   "nxt-sw-6-w",
		ListenAddr: "127.0.0.1:9090",
		VolumeRoot: t.TempDir(),
		Role:       "worker",
		Health:     func(context.Context, string) (int, error) { return 200, nil },
	}
	m, ok := find(RunNodeLocal(context.Background(), in), "cluster mirror")
	if !ok {
		t.Fatal("the worker report has no cluster mirror line")
	}
	if m.Status != StatusSkip {
		t.Fatalf("worker mirror check = %+v, want SKIP", m)
	}
	if !strings.Contains(m.Detail, "worker") {
		t.Errorf("the skip must say why: %s", m.Detail)
	}
}

// TestRunNodeLocalNamesWhatTheMirrorCannotAnswer: the old blanket "not answered here" is
// now a specific list, and the reason secrets decryption is on it is the cipher key — the
// thing slice 5 provisions and the snapshot deliberately never carries.
func TestRunNodeLocalNamesWhatTheMirrorCannotAnswer(t *testing.T) {
	rep := RunNodeLocal(context.Background(), nodeInputWithMirror(t, mirrorStore(t, 1_700_000_000), time.Unix(1_700_000_042, 0)))
	c, ok := find(rep, "cluster state")
	if !ok {
		t.Fatal("the report must still name what it does not answer")
	}
	if c.Status != StatusSkip {
		t.Fatalf("cluster state = %+v, want SKIP", c)
	}
	for _, want := range []string{"cipher key", "stacks", "control-plane kit", "--api-url"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("the skip must name %q: %s", want, c.Detail)
		}
	}
	// It must NOT still claim settings are unavailable — that is the sentence slice 3
	// deleted, and leaving it would contradict the check right above it.
	if strings.Contains(c.Detail, "settings") {
		t.Errorf("the skip still claims settings are not answered here: %s", c.Detail)
	}
}

// TestRunNodeLocalSwarmLineTellsTheTruthPerRole: the old line told a follower MANAGER that
// the node list is manager-only, which is false — it can read it. Only the authoritative
// quorum view belongs to the leader.
func TestRunNodeLocalSwarmLineTellsTheTruthPerRole(t *testing.T) {
	manager, _ := find(RunNodeLocal(context.Background(), nodeInputWithMirror(t, mirrorStore(t, 1_700_000_000), time.Unix(1_700_000_042, 0))), "swarm")
	if !strings.Contains(manager.Detail, "readable from a manager") {
		t.Errorf("a follower manager must be told the node list is readable: %s", manager.Detail)
	}
	workerIn := NodeInput{Hostname: "w", VolumeRoot: t.TempDir(), Role: "worker",
		Health: func(context.Context, string) (int, error) { return 200, nil }}
	worker, _ := find(RunNodeLocal(context.Background(), workerIn), "swarm")
	if !strings.Contains(worker.Detail, "worker") || !strings.Contains(worker.Detail, "manager-only") {
		t.Errorf("a worker must be told the call is manager-only: %s", worker.Detail)
	}
}
