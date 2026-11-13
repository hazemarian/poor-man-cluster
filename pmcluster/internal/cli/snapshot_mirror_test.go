package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/clustersnapshot"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func mirrorStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// mirrorFake is a scripted Fetch: it answers with the configured result and records
// what the loop sent, which is half of what these tests are about.
//
// Every field is behind the mutex because the loop tests read the recorder from the
// test goroutine while the loop writes it from its own — with -race (which CI runs and a
// plain `go test` does not) that is a test failure, and it was one.
type mirrorFake struct {
	mu     sync.Mutex
	res    clustersnapshot.Result
	err    error
	calls  int
	sentIn []string
}

func (f *mirrorFake) fetch(_ context.Context, ifNoneMatch string) (clustersnapshot.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.sentIn = append(f.sentIn, ifNoneMatch)
	if f.err != nil {
		return clustersnapshot.Result{}, f.err
	}
	return f.res, nil
}

// set swaps the scripted answer between calls.
func (f *mirrorFake) set(res clustersnapshot.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.res = res
}

// counts returns how many polls happened and what each one sent, as a copy.
func (f *mirrorFake) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]string(nil), f.sentIn...)
}

func depsFor(f *mirrorFake, leads bool) mirrorDeps {
	return mirrorDeps{
		Store: func() (*store.Store, error) { return nil, errors.New("unused in tick tests") },
		Fetch: f.fetch,
		Leads: func(context.Context) bool { return leads },
		Now:   func() time.Time { return time.Unix(1_700_000_500, 0) },
		Log:   zerolog.Nop(),
	}
}

func snapAt(version int64, key, value string) clustersnapshot.Result {
	return clustersnapshot.Result{
		Snapshot: &store.Snapshot{
			Version:     version,
			GeneratedAt: 1_700_000_000,
			Settings:    []store.SnapshotSetting{{Key: key, Value: value}},
			Secrets:     []store.SnapshotSecret{},
			Users:       []store.SnapshotUser{},
		},
		Leader: "nxt-sw-5-m",
		ETag:   clustersnapshot.ETag("nxt-sw-5-m", version),
	}
}

// TestMirrorTickAppliesTheSnapshotsAndRecordsThePair: one tick takes the leader's rows
// into this node's store and records (leader, version) — the pair the next poll sends
// back as an ETag. The first poll is unconditional, because there is nothing applied
// to match against.
func TestMirrorTickAppliesTheSnapshotsAndRecordsThePair(t *testing.T) {
	st := mirrorStore(t)
	fake := &mirrorFake{res: snapAt(4, "domain", "example.test")}

	if err := mirrorTick(context.Background(), depsFor(fake, false), st); err != nil {
		t.Fatalf("mirrorTick: %v", err)
	}
	calls, sent := fake.counts()
	if calls != 1 || sent[0] != "" {
		t.Fatalf("first poll sent If-None-Match %q, want the unconditional read", sent[0])
	}
	if got := st.SettingDefault(context.Background(), "domain", ""); got != "example.test" {
		t.Errorf("setting = %q, want the mirrored value", got)
	}
	state, err := st.SyncState(context.Background())
	if err != nil {
		t.Fatalf("SyncState: %v", err)
	}
	if state.Leader != "nxt-sw-5-m" || state.Version != 4 || state.AppliedAt != 1_700_000_500 || state.GeneratedAt != 1_700_000_000 {
		t.Errorf("sync state = %+v", *state)
	}

	// The second poll must send exactly the pair that was applied. A bare version would
	// match another leader's identical number after a failover and answer 304 for state
	// this node never applied.
	fake.set(clustersnapshot.Result{NotModified: true})
	if err := mirrorTick(context.Background(), depsFor(fake, false), st); err != nil {
		t.Fatalf("second mirrorTick: %v", err)
	}
	_, sent = fake.counts()
	if want := clustersnapshot.ETag("nxt-sw-5-m", 4); sent[1] != want {
		t.Errorf("second poll sent %q, want %q", sent[1], want)
	}
}

// TestMirrorTickDoesNotFetchWhenThisNodeLeads is the trap written as a test: the leader
// reads its own store, and a second path applying a different answer to the same rows
// is a race, not redundancy.
func TestMirrorTickDoesNotFetchWhenThisNodeLeads(t *testing.T) {
	st := mirrorStore(t)
	fake := &mirrorFake{res: snapAt(1, "domain", "x")}
	if err := mirrorTick(context.Background(), depsFor(fake, true), st); err != nil {
		t.Fatalf("mirrorTick: %v", err)
	}
	if calls, _ := fake.counts(); calls != 0 {
		t.Fatalf("the leader fetched the snapshot %d time(s); it must read its own store", calls)
	}
	if _, err := st.SyncState(context.Background()); !errors.Is(err, store.ErrNeverSynced) {
		t.Errorf("the leader recorded a sync state: %v", err)
	}
}

// TestMirrorTickKeepsTheLastSnapshotOnAFetchError: a poll that fails changes NOTHING.
// A mirror that empties itself on a transport error is how a failover becomes a blank
// console.
func TestMirrorTickKeepsTheLastSnapshotOnAFetchError(t *testing.T) {
	st := mirrorStore(t)
	if err := mirrorTick(context.Background(), depsFor(&mirrorFake{res: snapAt(4, "domain", "kept")}, false), st); err != nil {
		t.Fatalf("first mirrorTick: %v", err)
	}

	fail := &mirrorFake{err: errors.New("dial tcp: connection refused")}
	if err := mirrorTick(context.Background(), depsFor(fail, false), st); err == nil {
		t.Fatal("a failed poll returned no error (it would be a silent no-op)")
	}
	if got := st.SettingDefault(context.Background(), "domain", ""); got != "kept" {
		t.Errorf("setting = %q after a failed poll, want the last applied value", got)
	}
	state, err := st.SyncState(context.Background())
	if err != nil {
		t.Fatalf("SyncState: %v", err)
	}
	if state.Version != 4 {
		t.Errorf("sync version = %d after a failed poll, want 4", state.Version)
	}
}

// TestMirrorTickAppliesAcrossALeaderChange: the same version number from a DIFFERENT
// leader is different state. The ETag names the pair, so the poll is a 200 and the
// rows are applied; a version-only identity would answer 304 here and leave the mirror
// describing a leader that no longer exists.
func TestMirrorTickAppliesAcrossALeaderChange(t *testing.T) {
	st := mirrorStore(t)
	fake := &mirrorFake{res: snapAt(4, "domain", "from-a")}
	if err := mirrorTick(context.Background(), depsFor(fake, false), st); err != nil {
		t.Fatalf("first mirrorTick: %v", err)
	}

	// Leadership moved; the new leader happens to be at the same version.
	moved := snapAt(4, "domain", "from-b")
	moved.Leader = "nxt-sw-4-m"
	moved.ETag = clustersnapshot.ETag("nxt-sw-4-m", 4)
	fake2 := &mirrorFake{res: moved}
	if err := mirrorTick(context.Background(), depsFor(fake2, false), st); err != nil {
		t.Fatalf("second mirrorTick: %v", err)
	}
	want := clustersnapshot.ETag("nxt-sw-5-m", 4)
	if _, sent2 := fake2.counts(); sent2[0] != want {
		t.Errorf("poll after a leadership change sent %q, want the pair it applied %q", sent2[0], want)
	}
	if got := st.SettingDefault(context.Background(), "domain", ""); got != "from-b" {
		t.Errorf("setting = %q, want the new leader's value", got)
	}
	state, err := st.SyncState(context.Background())
	if err != nil {
		t.Fatalf("SyncState: %v", err)
	}
	if state.Leader != "nxt-sw-4-m" {
		t.Errorf("sync leader = %q, want the new leader", state.Leader)
	}
}

// TestOpenMirroredStoreRefusesToCreateADatabase: a follower that was never initialised
// must stay uninitialised. store.Open would create data.db (and its directory) on any
// path handed to it, which would make serve.go's "run pmcluster init" guard pass on a
// node that has no control plane — and the mirror would then serve an empty one.
func TestOpenMirroredStoreRefusesToCreateADatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nested", "data.db")
	if _, err := openMirroredStore(missing); err == nil {
		t.Fatal("openMirroredStore created a database: the uninitialised-node guard is defeated")
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Errorf("openMirroredStore left a file behind at %s", missing)
	}

	// An existing database opens normally.
	existing := filepath.Join(t.TempDir(), "data.db")
	seed, err := store.Open(existing)
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	st, err := openMirroredStore(existing)
	if err != nil {
		t.Fatalf("openMirroredStore on an existing database: %v", err)
	}
	_ = st.Close()
}

// TestStartSnapshotMirrorPollsThenStopsOnDemand: the loop applies at least one snapshot
// without waiting for the interval, and closing it is what makes the leader's store the
// only writer afterwards — so `done` must not close until the loop has really exited.
func TestStartSnapshotMirrorPollsThenStopsOnDemand(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "loop.db")
	fake := &mirrorFake{res: snapAt(2, "domain", "looped")}
	d := mirrorDeps{
		Store:    func() (*store.Store, error) { return store.Open(dbPath) },
		Fetch:    fake.fetch,
		Leads:    func(context.Context) bool { return false },
		Now:      func() time.Time { return time.Unix(1_700_000_500, 0) },
		Interval: 5 * time.Millisecond,
		Log:      zerolog.Nop(),
	}

	stop, done := startSnapshotMirror(context.Background(), d)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if calls, _ := fake.counts(); calls > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls, _ := fake.counts(); calls == 0 {
		stop()
		t.Fatal("the loop never polled: a follower would wait a full interval before its first snapshot")
	}
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("done did not close after stop: the loop would keep its store handle open while the leader opens its own")
	}

	// The applied rows survive the loop's shutdown — the handle is closed, not rolled back.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if got := st.SettingDefault(context.Background(), "domain", ""); got != "looped" {
		t.Errorf("setting after the loop stopped = %q, want the applied value", got)
	}
}
