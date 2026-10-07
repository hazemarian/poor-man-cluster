package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// fakeClient is a minimal runtime.Client over an in-memory config store.
type fakeClient struct {
	runtime.Client
	configs map[string]runtime.ConfigInspectResult
}

func newFake() *fakeClient {
	return &fakeClient{configs: map[string]runtime.ConfigInspectResult{}}
}

func (f *fakeClient) ConfigCreate(_ context.Context, spec runtime.ConfigSpec) error {
	f.configs[spec.Name] = runtime.ConfigInspectResult{Labels: spec.Labels, Data: spec.Data}
	return nil
}

func (f *fakeClient) ConfigRemove(_ context.Context, name string) error {
	delete(f.configs, name)
	return nil
}

func (f *fakeClient) ConfigList(_ context.Context, labelKey, labelValue string) ([]string, error) {
	var names []string
	for name, cfg := range f.configs {
		if labelKey == "" || cfg.Labels[labelKey] == labelValue {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (f *fakeClient) ConfigInspect(_ context.Context, name string) (runtime.ConfigInspectResult, error) {
	cfg, ok := f.configs[name]
	if !ok {
		return runtime.ConfigInspectResult{}, errors.New("config not found")
	}
	return cfg, nil
}

// writeKit drops a realistic data dir: data.db, .encryption_key, config.yaml,
// a config/ tree (with a cert), and a logs/ file that must never be snapshotted.
func writeKit(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"data.db":              "sqlite-bytes",
		".encryption_key":      "0123456789abcdef0123456789abcdef",
		"config.yaml":          "listen_addr: 0.0.0.0:9090",
		"config/main-cert.pem": "cert-bytes",
		"logs/old.log":         "noise",
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// tarEntries returns the entry names of a gzipped tar payload.
func tarEntries(t *testing.T, payload []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		var body []byte
		if hdr.Typeflag == tar.TypeReg {
			body = make([]byte, hdr.Size)
			n, rerr := tr.Read(body)
			// tar.Reader reports io.EOF once the entry's last byte is
			// consumed; a full read with io.EOF is a success.
			if rerr != nil && (rerr != io.EOF || n != len(body)) {
				t.Fatal(rerr)
			}
		}
		out[hdr.Name] = body
	}
	return out
}

func stateNames(t *testing.T, f *fakeClient) []string {
	t.Helper()
	names, err := f.ConfigList(context.Background(), LabelKind, KindState)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestSnapshot_CreatesBothConfigs(t *testing.T) {
	dataDir := t.TempDir()
	writeKit(t, dataDir)
	f := newFake()
	k := &Kit{Docker: f, DataDir: dataDir, Log: zerolog.Nop()}

	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}

	state := stateNames(t, f)
	keyNames, err := f.ConfigList(context.Background(), LabelKind, KindKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 || len(keyNames) != 1 {
		t.Fatalf("expected 1 state + 1 key config, got %d + %d", len(state), len(keyNames))
	}
	if !strings.HasPrefix(state[0], StateNamePrefix+"_") {
		t.Fatalf("unexpected state config name %q", state[0])
	}
	if !strings.HasPrefix(keyNames[0], KeyNamePrefix+"_") {
		t.Fatalf("unexpected key config name %q", keyNames[0])
	}

	keyInspect, err := f.ConfigInspect(context.Background(), keyNames[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(keyInspect.Data) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("key config carries wrong bytes: %q", keyInspect.Data)
	}
	if keyInspect.Labels[LabelKind] != KindKey || keyInspect.Labels[ManagedLabel] != "true" {
		t.Fatalf("key config labels wrong: %v", keyInspect.Labels)
	}

	stateInspect, err := f.ConfigInspect(context.Background(), state[0])
	if err != nil {
		t.Fatal(err)
	}
	if stateInspect.Labels[LabelKind] != KindState || stateInspect.Labels[LabelSnapshotAt] == "" {
		t.Fatalf("state config labels wrong: %v", stateInspect.Labels)
	}
	entries := tarEntries(t, stateInspect.Data)
	for _, want := range []string{"data.db", "config.yaml", "config/main-cert.pem"} {
		if _, ok := entries[want]; !ok {
			t.Fatalf("state config missing entry %q (have %v)", want, keys(entries))
		}
	}
	if _, ok := entries[".encryption_key"]; ok {
		t.Fatal("security split violated: state config contains the encryption key")
	}
	if _, ok := entries["logs/old.log"]; ok {
		t.Fatal("logs must not be snapshotted")
	}
}

func TestSnapshot_SkipsWhenUnchanged(t *testing.T) {
	dataDir := t.TempDir()
	writeKit(t, dataDir)
	f := newFake()
	k := &Kit{Docker: f, DataDir: dataDir, Log: zerolog.Nop()}

	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(stateNames(t, f)); n != 1 {
		t.Fatalf("unchanged kit must not re-snapshot; %d state configs exist", n)
	}
}

func TestSnapshot_SnapshotsWhenChanged(t *testing.T) {
	dataDir := t.TempDir()
	writeKit(t, dataDir)
	f := newFake()
	k := &Kit{Docker: f, DataDir: dataDir, Log: zerolog.Nop()}

	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Mutate data.db AFTER the first snapshot (its mtime must exceed the
	// first snapshot's timestamp).
	time.Sleep(2 * time.Millisecond)
	db := filepath.Join(dataDir, "data.db")
	if err := os.Chtimes(db, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(stateNames(t, f)); n != 2 {
		t.Fatalf("changed kit must re-snapshot; %d state configs exist", n)
	}
}

func TestSnapshot_PrunesOld(t *testing.T) {
	dataDir := t.TempDir()
	writeKit(t, dataDir)
	f := newFake()
	k := &Kit{Docker: f, DataDir: dataDir, Log: zerolog.Nop()}

	for i := 0; i < 4; i++ {
		if err := k.Snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
		db := filepath.Join(dataDir, "data.db")
		if err := os.Chtimes(db, time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(stateNames(t, f)); n != KeepSnapshots {
		t.Fatalf("prune must keep %d state configs, kept %d", KeepSnapshots, n)
	}
	keyNames, err := f.ConfigList(context.Background(), LabelKind, KindKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(keyNames) != KeepSnapshots {
		t.Fatalf("prune must keep %d key configs, kept %d", KeepSnapshots, len(keyNames))
	}
}

func TestSnapshot_RejectsOversizedKit(t *testing.T) {
	dataDir := t.TempDir()
	writeKit(t, dataDir)
	// Incompressible payload (gzip cannot shrink it) so the limit guard fires.
	rnd := rand.New(rand.NewSource(1))
	big := make([]byte, MaxConfigBytes+1)
	if _, err := rnd.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config", "huge.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFake()
	k := &Kit{Docker: f, DataDir: dataDir, Log: zerolog.Nop()}
	if err := k.Snapshot(context.Background()); err == nil {
		t.Fatal("oversized kit must fail loudly")
	}
	if n := len(stateNames(t, f)); n != 0 {
		t.Fatal("no config must be created for an oversized kit")
	}
}

func TestSnapshot_NilDockerNoop(t *testing.T) {
	dataDir := t.TempDir()
	writeKit(t, dataDir)
	k := &Kit{Docker: nil, DataDir: dataDir, Log: zerolog.Nop()}
	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRestore_RoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	writeKit(t, dataDir)
	f := newFake()
	k := &Kit{Docker: f, DataDir: dataDir, Log: zerolog.Nop()}
	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Simulate a node whose control plane was wiped.
	emptyDir := t.TempDir()
	k2 := &Kit{Docker: f, DataDir: emptyDir, Log: zerolog.Nop()}
	restored, err := k2.Restore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("expected restore on a wiped data dir")
	}
	if b, _ := os.ReadFile(filepath.Join(emptyDir, "data.db")); string(b) != "sqlite-bytes" {
		t.Fatalf("data.db not restored: %q", b)
	}
	if b, _ := os.ReadFile(k2.KeyPath()); string(b) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("encryption key not restored: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(emptyDir, "config", "main-cert.pem")); string(b) != "cert-bytes" {
		t.Fatalf("config tree not restored: %q", b)
	}
	// Restored data.db is timestamped at the snapshot → a second restore is a
	// no-op (no restore loop on every promotion).
	restored, err = k2.Restore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if restored {
		t.Fatal("second restore after a fresh restore must be a no-op")
	}
}

func TestRestore_KeepsCurrentDB(t *testing.T) {
	dataDir := t.TempDir()
	writeKit(t, dataDir)
	f := newFake()
	k := &Kit{Docker: f, DataDir: dataDir, Log: zerolog.Nop()}
	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Local DB + key are NEWER than the snapshot (shared-storage case): keep
	// them, restore nothing.
	curDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(curDir, "data.db"), []byte("live-db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(curDir, ".encryption_key"), []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	now := time.Now()
	if err := os.Chtimes(filepath.Join(curDir, "data.db"), now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(curDir, ".encryption_key"), now, now); err != nil {
		t.Fatal(err)
	}
	k2 := &Kit{Docker: f, DataDir: curDir, Log: zerolog.Nop()}
	restored, err := k2.Restore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if restored {
		t.Fatal("current local control plane must not be clobbered")
	}
	if b, _ := os.ReadFile(filepath.Join(curDir, "data.db")); string(b) != "live-db" {
		t.Fatal("current local DB was clobbered")
	}
}

func TestRestore_RestoresMissingKeyOnly(t *testing.T) {
	dataDir := t.TempDir()
	writeKit(t, dataDir)
	f := newFake()
	k := &Kit{Docker: f, DataDir: dataDir, Log: zerolog.Nop()}
	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}

	// DB current, key file missing (fresh join that never got the key).
	curDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(curDir, "data.db"), []byte("sqlite-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	k2 := &Kit{Docker: f, DataDir: curDir, Log: zerolog.Nop()}
	restored, err := k2.Restore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("expected the missing key to be restored")
	}
	if b, _ := os.ReadFile(k2.KeyPath()); string(b) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("key not restored: %q", b)
	}
}

func TestRestore_MissingKeyConfigErrors(t *testing.T) {
	dataDir := t.TempDir()
	writeKit(t, dataDir)
	f := newFake()
	k := &Kit{Docker: f, DataDir: dataDir, Log: zerolog.Nop()}
	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Delete the key config: the data kit is now ciphertext without its key.
	keyNames, err := f.ConfigList(context.Background(), LabelKind, KindKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range keyNames {
		if err := f.ConfigRemove(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}

	k2 := &Kit{Docker: f, DataDir: t.TempDir(), Log: zerolog.Nop()}
	_, err = k2.Restore(context.Background())
	if err == nil {
		t.Fatal("restore must refuse a state config whose key config is missing")
	}
}

func TestRestore_NoStateConfigs(t *testing.T) {
	f := newFake()
	k := &Kit{Docker: f, DataDir: t.TempDir(), Log: zerolog.Nop()}
	restored, err := k.Restore(context.Background())
	if err == nil || !errors.Is(err, ErrNoSnapshots) {
		t.Fatalf("expected ErrNoSnapshots, got %v", err)
	}
	if restored {
		t.Fatal("nothing to restore")
	}
}

func TestRestore_NilDockerErrNoSnapshots(t *testing.T) {
	k := &Kit{Docker: nil, DataDir: t.TempDir(), Log: zerolog.Nop()}
	if _, err := k.Restore(context.Background()); !errors.Is(err, ErrNoSnapshots) {
		t.Fatalf("nil docker must fall back (ErrNoSnapshots), got %v", err)
	}
}

// workerClient is a runtime.Client whose config operations fail with Docker's
// "This node is not a swarm manager" error — a worker node cannot read or
// write swarm configs.
type workerClient struct {
	runtime.Client
}

// errNotSwarmManager reproduces Docker's real daemon message verbatim; the
// production code matches on the substring, so the fixture must keep it exact.
//
//nolint:staticcheck // ST1005: fidelity to the real docker error string wins
var errNotSwarmManager = errors.New("Error response from daemon: This node is not a swarm manager. Worker nodes can't be used to view or modify cluster state. Please run this command on a manager node or promote the current node to a manager.")

func (workerClient) ConfigList(context.Context, string, string) ([]string, error) {
	return nil, errNotSwarmManager
}

func (workerClient) ConfigInspect(context.Context, string) (runtime.ConfigInspectResult, error) {
	return runtime.ConfigInspectResult{}, errNotSwarmManager
}

func (workerClient) ConfigCreate(context.Context, runtime.ConfigSpec) error {
	return errNotSwarmManager
}
func (workerClient) ConfigRemove(context.Context, string) error { return errNotSwarmManager }

func TestRestore_WorkerNodeFallsBack(t *testing.T) {
	// A worker node (not a swarm manager) cannot list state configs. Restore
	// must treat that as ErrNoSnapshots so the caller falls back to the
	// tarball archive path — never a fatal startup error.
	k := &Kit{Docker: workerClient{}, DataDir: t.TempDir(), Log: zerolog.Nop()}
	if _, err := k.Restore(context.Background()); !errors.Is(err, ErrNoSnapshots) {
		t.Fatalf("worker node must fall back (ErrNoSnapshots), got %v", err)
	}
}

func TestSnapshot_WorkerNodeNoop(t *testing.T) {
	// A worker node cannot list/create configs; Snapshot must no-op rather
	// than return an error (the loop logs it as fatal-ish noise otherwise).
	dataDir := t.TempDir()
	writeKit(t, dataDir)
	k := &Kit{Docker: workerClient{}, DataDir: dataDir, Log: zerolog.Nop()}
	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatalf("worker node snapshot must no-op, got %v", err)
	}
}

// leaderlessClient is a runtime.Client whose config operations fail with the
// quorum-loss error Docker returns while the Swarm is briefly without a Raft
// leader. Unlike the worker case this is *transient* — the managers are up but
// fewer than half can see each other (BUG-020, seen live in TC10-A: dockerd was
// stopped on the leader and a second manager dropped off the network, so no
// manager had quorum and every daemon that restarted crash-looped).
type leaderlessClient struct {
	runtime.Client
}

// errNoLeader reproduces Docker's real daemon message verbatim; production
// matches on the substring, so the fixture must keep it exact.
//
//nolint:staticcheck // ST1005: fidelity to the real docker error string wins
var errNoLeader = errors.New("Error response from daemon: rpc error: code = Unknown desc = The swarm does not have a leader. It's possible that too few managers are online. Make sure more than half of the managers are online.")

func (leaderlessClient) ConfigList(context.Context, string, string) ([]string, error) {
	return nil, errNoLeader
}

func (leaderlessClient) ConfigInspect(context.Context, string) (runtime.ConfigInspectResult, error) {
	return runtime.ConfigInspectResult{}, errNoLeader
}

func (leaderlessClient) ConfigCreate(context.Context, runtime.ConfigSpec) error {
	return errNoLeader
}

func (leaderlessClient) ConfigRemove(context.Context, string) error { return errNoLeader }

func TestIsSwarmUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"worker node", errNotSwarmManager, true},
		{"no raft leader", errNoLeader, true},
		{"too few managers", errors.New("rpc error: ... too few managers are online"), true},
		{"missing config", errors.New("config not found: foo_v1"), false},
		{"permission", errors.New("permission denied"), false},
	}
	for _, tc := range cases {
		if got := IsSwarmUnavailable(tc.err); got != tc.want {
			t.Errorf("%s: IsSwarmUnavailable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRestore_LeaderlessSwarmFallsBack(t *testing.T) {
	// No quorum: the replicated kit cannot be read right now. Restore must
	// degrade to ErrNoSnapshots (tarball fallback) rather than returning a
	// fatal error that exits the daemon and crash-loops it (BUG-020).
	k := &Kit{Docker: leaderlessClient{}, DataDir: t.TempDir(), Log: zerolog.Nop()}
	if _, err := k.Restore(context.Background()); !errors.Is(err, ErrNoSnapshots) {
		t.Fatalf("leaderless swarm must fall back (ErrNoSnapshots), got %v", err)
	}
}

func TestSnapshot_LeaderlessSwarmNoop(t *testing.T) {
	// Same transient condition for Snapshot: no-op so the leader loop keeps
	// ticking instead of logging an error on every pass.
	dataDir := t.TempDir()
	writeKit(t, dataDir)
	k := &Kit{Docker: leaderlessClient{}, DataDir: dataDir, Log: zerolog.Nop()}
	if err := k.Snapshot(context.Background()); err != nil {
		t.Fatalf("leaderless swarm snapshot must no-op, got %v", err)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
