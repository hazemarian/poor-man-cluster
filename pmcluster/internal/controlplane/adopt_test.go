package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// clusterKey is the encryption key the fake cluster's kit carries.
const clusterKey = "0123456789abcdef0123456789abcdef"

// countingDocker wraps the in-memory config fake and counts the reads the
// freshness predicate makes, so a test can assert HOW MANY checks a leadership
// transition performed — not merely that one happened.
type countingDocker struct {
	*fakeClient
	// stateLists counts ConfigList calls for the state config family: one per
	// freshness check.
	stateLists int
}

func newCountingDocker() *countingDocker { return &countingDocker{fakeClient: newFake()} }

func (c *countingDocker) ConfigList(ctx context.Context, labelKey, labelValue string) ([]string, error) {
	if labelKey == LabelKind && labelValue == KindState {
		c.stateLists++
	}
	return c.fakeClient.ConfigList(ctx, labelKey, labelValue)
}

// swapRecorder counts how many times the adopter asked for the store to be
// swapped (i.e. how many extractions happened) and, optionally, performs the
// real swap on a live *store.Store.
type swapRecorder struct {
	swaps  int
	dbPath string
	// atEntry is the data.db content seen when Swap was entered — i.e. before
	// the promotion ran. It proves the kit was extracted alongside, not in place.
	atEntry []byte
	// inner is the real store handle when the test needs the close/reopen.
	inner StoreSwapper
	// failWith makes the promotion claim to fail without touching files.
	failWith error
}

func (s *swapRecorder) Swap(fn func() error) error {
	s.swaps++
	if s.dbPath != "" {
		s.atEntry, _ = os.ReadFile(s.dbPath)
	}
	if s.failWith != nil {
		return s.failWith
	}
	if s.inner != nil {
		return s.inner.Swap(fn)
	}
	return fn()
}

// clusterWithKit builds a source node with a real SQLite store holding one
// setting, publishes it as the cluster's newest snapshot, and returns the
// Docker fake plus the snapshot time.
func clusterWithKit(t *testing.T, probe string) (*countingDocker, time.Time) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	if err := st.SetSetting(ctx, "probe", probe); err != nil {
		t.Fatalf("seed source setting: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close source store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".encryption_key"), []byte(clusterKey), 0o600); err != nil {
		t.Fatal(err)
	}
	dc := newCountingDocker()
	src := &Kit{Docker: dc, DataDir: dir, Log: zerolog.Nop()}
	if err := src.Snapshot(ctx); err != nil {
		t.Fatalf("publish source kit: %v", err)
	}
	names, err := dc.ConfigList(ctx, LabelKind, KindState)
	if err != nil || len(names) != 1 {
		t.Fatalf("expected exactly one state config, got %v (%v)", names, err)
	}
	inspect, err := dc.ConfigInspect(ctx, names[0])
	if err != nil {
		t.Fatal(err)
	}
	at, err := parseSnapshotAt(inspect.Labels)
	if err != nil {
		t.Fatal(err)
	}
	// The Snapshot above was a pre-existing check of its own; reset the counter
	// so tests measure only the transitions they trigger.
	dc.stateLists = 0
	return dc, at
}

// staleNode returns a data dir with a real store, the cluster's key and a
// revision marker far in the past — the state of a manager promoted while
// serving its own older database.
func staleNode(t *testing.T) (string, *store.Store) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open stale store: %v", err)
	}
	if err := st.SetSetting(ctx, "probe", "local-stale"); err != nil {
		t.Fatalf("seed stale setting: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".encryption_key"), []byte(clusterKey), 0o600); err != nil {
		t.Fatal(err)
	}
	k := &Kit{Docker: nil, DataDir: dir, Log: zerolog.Nop()}
	if err := k.writeRevision(time.Now().Add(-72 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	return dir, st
}

// (a) A stale revision plus leadership gain replaces the store, advances the
// revision, and leaves the long-lived *store.Store usable on the adopted DB.
func TestAdoptOnLeadership_StaleRevisionReplacesStore(t *testing.T) {
	ctx := context.Background()
	dc, at := clusterWithKit(t, "from-cluster")
	dir, st := staleNode(t)
	defer st.Close()

	k := &Kit{Docker: dc, DataDir: dir, Log: zerolog.Nop()}
	swap := &swapRecorder{dbPath: filepath.Join(dir, "data.db"), inner: st}

	adoption, err := k.AdoptOnLeadership(ctx, swap)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !adoption.Adopted {
		t.Fatal("a node behind the newest snapshot must adopt on leadership gain")
	}
	if swap.swaps != 1 {
		t.Fatalf("expected exactly one store swap, got %d", swap.swaps)
	}
	if !adoption.Revision.Equal(at) {
		t.Fatalf("adoption revision = %v, want the snapshot time %v", adoption.Revision, at)
	}
	// The marker advanced: the next check is a no-op.
	if got := k.readRevision(); !got.Equal(at) {
		t.Fatalf("revision marker = %v, want %v", got, at)
	}
	// The store handle every consumer already holds now serves the cluster's DB.
	probe, err := st.Setting(ctx, "probe")
	if err != nil {
		t.Fatalf("read probe through the reopened store: %v", err)
	}
	if probe != "from-cluster" {
		t.Fatalf("probe = %q, want the adopted value", probe)
	}
	// The replace happened by rename, not by truncating the live file: at the
	// moment the store was closed, data.db still held the node's own rows.
	if !bytes.Contains(swap.atEntry, []byte("local-stale")) {
		t.Fatalf("data.db did not hold the previous database when the store was closed for the swap (%d bytes)", len(swap.atEntry))
	}
	// A write still works through the same handle.
	if err := st.SetSetting(ctx, "probe2", "written-after-adopt"); err != nil {
		t.Fatalf("write through the reopened store: %v", err)
	}
}

// (b) A current revision does no work at all: no extraction, no store swap, no
// touched file.
func TestAdoptOnLeadership_CurrentRevisionDoesNotExtract(t *testing.T) {
	ctx := context.Background()
	dc, at := clusterWithKit(t, "from-cluster")

	// A node that already synced: the cluster's key, its own DB and the
	// snapshot's revision marker.
	dir, st := staleNode(t)
	defer st.Close()
	k := &Kit{Docker: dc, DataDir: dir, Log: zerolog.Nop()}
	if err := k.writeRevision(at); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	swap := &swapRecorder{dbPath: filepath.Join(dir, "data.db"), inner: st}

	adoption, err := k.AdoptOnLeadership(ctx, swap)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if adoption.Adopted {
		t.Fatal("a node at the newest snapshot must not adopt")
	}
	if swap.swaps != 0 {
		t.Fatalf("expected zero store swaps, got %d", swap.swaps)
	}
	after, err := os.ReadFile(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a current control plane was re-extracted over")
	}
}

// (c) Two transitions in a row: the check runs twice (once per transition), the
// extraction only once.
func TestAdoptOnLeadership_ChecksOncePerTransition(t *testing.T) {
	ctx := context.Background()
	dc, _ := clusterWithKit(t, "from-cluster")
	dir, st := staleNode(t)
	defer st.Close()

	k := &Kit{Docker: dc, DataDir: dir, Log: zerolog.Nop()}
	swap := &swapRecorder{dbPath: filepath.Join(dir, "data.db"), inner: st}

	first, err := k.AdoptOnLeadership(ctx, swap)
	if err != nil {
		t.Fatalf("first transition: %v", err)
	}
	second, err := k.AdoptOnLeadership(ctx, swap)
	if err != nil {
		t.Fatalf("second transition: %v", err)
	}
	if !first.Adopted {
		t.Fatal("the first transition must adopt (the node was behind)")
	}
	if second.Adopted {
		t.Fatal("the second transition must be a no-op: leadership was lost and regained, nothing changed")
	}
	if dc.stateLists != 2 {
		t.Fatalf("expected exactly two freshness checks (one per transition), got %d", dc.stateLists)
	}
	if swap.swaps != 1 {
		t.Fatalf("expected exactly one extraction across two transitions, got %d", swap.swaps)
	}
}

// (d) A state config without its key config is refused: the current database
// keeps serving and the refusal is logged at error level.
func TestAdoptOnLeadership_MissingKeyConfigRefuses(t *testing.T) {
	ctx := context.Background()
	dc, _ := clusterWithKit(t, "from-cluster")
	dir, st := staleNode(t)
	defer st.Close()

	for _, name := range mustList(t, dc, KindKey) {
		if err := dc.ConfigRemove(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	k := &Kit{Docker: dc, DataDir: dir, Log: zerolog.New(&buf)}
	swap := &swapRecorder{dbPath: filepath.Join(dir, "data.db"), inner: st}

	adoption, err := k.AdoptOnLeadership(ctx, swap)
	if err == nil {
		t.Fatal("adopting a ciphertext without its key must be refused")
	}
	if adoption.Adopted {
		t.Fatal("a refused adoption must not report itself as adopted")
	}
	if swap.swaps != 0 {
		t.Fatalf("the store must not be closed for a refused adoption, got %d swaps", swap.swaps)
	}
	after, err := os.ReadFile(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the current database must survive a refused adoption")
	}
	assertLoggedAtError(t, buf.Bytes())
	assertNoStagingLeft(t, dir)
}

// (d2) An unreadable kit is refused the same way — extraction happens beside the
// data dir, so even a payload that does not untar cannot truncate data.db.
func TestAdoptOnLeadership_UnreadableKitKeepsDatabase(t *testing.T) {
	ctx := context.Background()
	dc, at := clusterWithKit(t, "from-cluster")
	// Publish a newer state config whose payload is not a tarball, with the key
	// config it needs so the refusal comes from the extraction, not the split.
	ts := strconv.FormatInt(at.Add(time.Minute).UnixNano(), 10)
	name := StateNamePrefix + "_" + ts
	dc.configs[name] = runtime.ConfigInspectResult{
		Labels: map[string]string{
			ManagedLabel:    "true",
			LabelKind:       KindState,
			LabelSnapshotAt: at.Add(time.Minute).Format(time.RFC3339Nano),
			LabelVersion:    "test",
		},
		Data: []byte("this is not a gzipped tar"),
	}
	dc.configs[KeyNamePrefix+"_"+ts] = runtime.ConfigInspectResult{
		Labels: map[string]string{ManagedLabel: "true", LabelKind: KindKey, LabelSnapshotAt: at.Add(time.Minute).Format(time.RFC3339Nano)},
		Data:   []byte(clusterKey),
	}

	dir, st := staleNode(t)
	defer st.Close()
	before, err := os.ReadFile(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	k := &Kit{Docker: dc, DataDir: dir, Log: zerolog.New(&buf)}
	swap := &swapRecorder{dbPath: filepath.Join(dir, "data.db"), inner: st}
	if _, err := k.AdoptOnLeadership(ctx, swap); err == nil {
		t.Fatal("an unreadable kit must be refused")
	}
	if swap.swaps != 0 {
		t.Fatalf("the store must not be closed for an unreadable kit, got %d swaps", swap.swaps)
	}
	after, err := os.ReadFile(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("an unreadable kit must not touch data.db")
	}
	assertLoggedAtError(t, buf.Bytes())
	assertNoStagingLeft(t, dir)
}

// (e) Tick churn on an up-to-date node never adopts: the leader's periodic
// snapshot ticks must not close the store or re-extract anything.
func TestAdoptOnLeadership_TickChurnNeverAdopts(t *testing.T) {
	ctx := context.Background()
	dc, at := clusterWithKit(t, "from-cluster")
	dir, st := staleNode(t)
	defer st.Close()
	k := &Kit{Docker: dc, DataDir: dir, Log: zerolog.Nop()}
	if err := k.writeRevision(at); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	swap := &swapRecorder{dbPath: filepath.Join(dir, "data.db"), inner: st}

	// Kit.Loop's tick body, run several times. Adoption is not part of it: the
	// only caller is the leadership transition.
	for i := 0; i < 5; i++ {
		if err := k.Snapshot(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if swap.swaps != 0 {
		t.Fatalf("tick churn must never swap the store, got %d swaps", swap.swaps)
	}
	after, err := os.ReadFile(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("tick churn re-extracted the database")
	}
}

// (f) A snapshot this node published itself is never adopted, even when its
// revision marker is gone (a failed marker write must not turn into a pointless
// self-restore of the state that was just published).
func TestAdoptOnLeadership_IgnoresOwnPublishedSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.db"), []byte("own-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".encryption_key"), []byte(clusterKey), 0o600); err != nil {
		t.Fatal(err)
	}
	dc := newCountingDocker()
	k := &Kit{Docker: dc, DataDir: dir, Log: zerolog.Nop()}
	if err := k.Snapshot(ctx); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if k.publishedConfig() == "" {
		t.Fatal("Snapshot must remember the config it published")
	}
	if err := os.Remove(k.RevisionPath()); err != nil {
		t.Fatal(err)
	}
	swap := &swapRecorder{dbPath: filepath.Join(dir, "data.db")}

	adoption, err := k.AdoptOnLeadership(ctx, swap)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if adoption.Adopted {
		t.Fatal("a node must not adopt the snapshot it published itself")
	}
	if swap.swaps != 0 {
		t.Fatalf("expected no store swap for its own snapshot, got %d", swap.swaps)
	}
}

// A swarm that is momentarily unreadable (quorum lost / worker) is not an
// adoption failure: nothing to adopt, nothing to crash.
func TestAdoptOnLeadership_SwarmUnavailableIsQuiet(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	k := &Kit{Docker: leaderlessClient{}, DataDir: dir, Log: zerolog.New(&buf)}
	swap := &swapRecorder{}
	adoption, err := k.AdoptOnLeadership(context.Background(), swap)
	if err != nil {
		t.Fatalf("a transient swarm outage must not fail adoption: %v", err)
	}
	if adoption.Adopted || swap.swaps != 0 {
		t.Fatal("nothing may be adopted while the Raft configs are unreadable")
	}
}

func mustList(t *testing.T, dc *countingDocker, kind string) []string {
	t.Helper()
	names, err := dc.ConfigList(context.Background(), LabelKind, kind)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// assertLoggedAtError requires at least one ERROR-level record carrying an
// error, so a refused adoption is loud rather than silent.
func assertLoggedAtError(t *testing.T, raw []byte) {
	t.Helper()
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON (%v): %s", err, line)
		}
		if rec["level"] == "error" && rec["error"] != nil && rec["error"] != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a refused adoption must log an error with a reason; log was:\n%s", raw)
	}
}

// assertNoStagingLeft proves the staging directory is cleaned up on every path:
// a leftover would be a half-extracted kit inside the data dir.
func assertNoStagingLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagingPrefix) {
			t.Fatalf("staging entry %q left behind in the data dir", e.Name())
		}
	}
}
