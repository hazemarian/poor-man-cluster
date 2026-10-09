package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/controlplane"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// leaderFake embeds runtime.Client so every method is satisfied, and lets the
// test override just NodeList (the only one the leader checks use).
type leaderFake struct {
	runtime.Client
	nodes   []runtime.Node
	nodeErr error
	info    runtime.Info
	infoErr error
}

func (f *leaderFake) NodeList(context.Context) ([]runtime.Node, error) {
	return f.nodes, f.nodeErr
}

// Info defaults to "not a swarm worker" (inactive) so existing standalone/
// leader tests keep today's behaviour; the worker test overrides it.
func (f *leaderFake) Info(context.Context) (runtime.Info, error) {
	return f.info, f.infoErr
}

// leaderFakeMutex is leaderFake with a mutex so a test can flip leadership
// while WatchSwarmLeadership's poll goroutine is reading the node list.
type leaderFakeMutex struct {
	runtime.Client
	mu    sync.Mutex
	nodes []runtime.Node
}

func (f *leaderFakeMutex) NodeList(context.Context) ([]runtime.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.Node(nil), f.nodes...), nil
}

// Info is unused by the mutex fake's tests; report a non-worker.
func (f *leaderFakeMutex) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{}, nil
}

func (f *leaderFakeMutex) setLeader(hostname string, leader bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.nodes {
		if f.nodes[i].Hostname == hostname {
			f.nodes[i].IsLeader = leader
		}
	}
}

func TestIsSwarmLeader_HappyPath(t *testing.T) {
	f := &leaderFake{nodes: []runtime.Node{
		{ID: "n1", Hostname: "mgr1", IsLeader: false},
		{ID: "n2", Hostname: "mgr2", IsLeader: true},
	}}
	leader, found, err := isSwarmLeader(context.Background(), f, "mgr2")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected mgr2 to be found")
	}
	if !leader {
		t.Fatal("expected mgr2 to be leader")
	}
	notLeader, found, err := isSwarmLeader(context.Background(), f, "mgr1")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected mgr1 to be found")
	}
	if notLeader {
		t.Fatal("expected mgr1 to not be leader")
	}
}

// TestWatchSwarmLeadership_StableLeaderEmitsOnce guards against a regression
// where a stable leader re-emitted true on every poll (cancelling + restarting
// the reconcile + control-plane snapshot loops every leaderPollInterval). The
// channel must emit exactly one true, then stay silent while leadership is
// unchanged.
func TestWatchSwarmLeadership_StableLeaderEmitsOnce(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	f := &leaderFake{nodes: []runtime.Node{
		{ID: "n1", Hostname: host, IsLeader: true},
		{ID: "n2", Hostname: "mgr2", IsLeader: false},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := WatchSwarmLeadership(ctx, f, zerolog.Nop())

	// First value arrives immediately (initial emit).
	first := <-ch
	if !first {
		t.Fatal("expected initial leader-true emit")
	}

	// Over several poll intervals (2 * leaderPollInterval), the channel must
	// NOT emit again while the node stays leader.
	select {
	case v := <-ch:
		t.Fatalf("stable leader re-emitted on poll: got %v (regression: loops restart every poll)", v)
	case <-time.After(2*leaderPollInterval + 2*time.Second):
		// correct — no repeat emit
	}
}

// TestWatchSwarmLeadership_LeaderToNotLeaderReEmits ensures the channel
// re-emits when leadership state actually changes (not just poll repeats).
func TestWatchSwarmLeadership_LeaderToNotLeaderReEmits(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	flip := &leaderFakeMutex{nodes: []runtime.Node{
		{ID: "n1", Hostname: host, IsLeader: true},
		{ID: "n2", Hostname: "mgr2", IsLeader: false},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := WatchSwarmLeadership(ctx, flip, zerolog.Nop())

	first := <-ch
	if !first {
		t.Fatal("expected initial leader-true emit")
	}

	// Flip leadership after the first poll has settled.
	time.Sleep(leaderPollInterval + time.Second)
	flip.setLeader(host, false)
	flip.setLeader("mgr2", true)

	select {
	case v := <-ch:
		if v {
			t.Fatalf("expected leader-loss emit (false), got %v", v)
		}
	case <-time.After(leaderPollInterval + 2*time.Second):
		t.Fatal("expected a leadership-loss emit after the flip")
	}
}

func TestIsSwarmLeader_NodeNotFound(t *testing.T) {
	f := &leaderFake{nodes: []runtime.Node{
		{ID: "n1", Hostname: "mgr1", IsLeader: true},
	}}
	leader, found, err := isSwarmLeader(context.Background(), f, "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected ghost not to be found")
	}
	if leader {
		t.Fatal("expected ghost not to be leader")
	}
}

func TestWaitForSwarmLeadership_NodeListErrorServesImmediately(t *testing.T) {
	// Docker unreachable (NodeList errors) must NOT block — standalone/local
	// mode serves right away instead of standing by forever.
	f := &leaderFake{nodeErr: errors.New("Cannot connect to the Docker daemon")}
	if err := waitForSwarmLeadership(context.Background(), f, zerolog.Nop()); err != nil {
		t.Fatalf("expected immediate return on NodeList error, got %v", err)
	}
}

func TestWaitForSwarmLeadership_NodeNotInSwarmServesImmediately(t *testing.T) {
	// A host that is not a swarm member (no node with our hostname) must not
	// block — treat it as standalone/local.
	f := &leaderFake{nodes: []runtime.Node{{ID: "n1", Hostname: "someone-else", IsLeader: true}}}
	if err := waitForSwarmLeadership(context.Background(), f, zerolog.Nop()); err != nil {
		t.Fatalf("expected immediate return for non-member, got %v", err)
	}
}

func TestWaitForSwarmLeadership_NonLeaderStandby(t *testing.T) {
	// A confirmed non-leader manager (this host in the node list, not leader)
	// blocks in standby until cancelled.
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &leaderFake{nodes: []runtime.Node{{ID: "n1", Hostname: host, IsLeader: false}}}
	done := make(chan error, 1)
	go func() {
		done <- waitForSwarmLeadership(ctx, f, zerolog.Nop())
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("expected standby to block, returned early: %v", err)
	default:
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("expected context.Canceled after cancel, got %v", err)
	}
}

// TestWaitForSwarmLeadership_SwarmWorkerStandsBy is the BUG-033 regression: a
// genuine swarm worker (swarm active, ControlAvailable false) must stand by
// (block) rather than serve + start a doomed control loop, logging the standby
// message exactly once.
func TestWaitForSwarmLeadership_SwarmWorkerStandsBy(t *testing.T) {
	f := &leaderFake{
		nodeErr: errors.New("This node is not a swarm manager."),
		info:    runtime.Info{SwarmLocalNodeState: "active", SwarmControlAvailable: false},
	}
	var buf bytes.Buffer
	log := zerolog.New(&buf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- waitForSwarmLeadership(ctx, f, log) }()

	// Stand by: it must block rather than return immediately.
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("swarm worker returned early (%v), want standby", err)
	default:
	}

	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("expected context.Canceled after cancel, got %v", err)
	}
	if got := strings.Count(buf.String(), "this node is a swarm worker — standing by (no control loop)"); got != 1 {
		t.Errorf("standby message logged %d times, want 1", got)
	}
}

// TestWatchSwarmLeadership_SwarmWorkerStandsByIdle is the BUG-033 regression
// on the reconcile-loop driver: a swarm worker must NOT emit leader-true (so
// the loop never starts), must log the standby message exactly once, and must
// keep re-checking so a later promotion to manager starts the loop.
func TestWatchSwarmLeadership_SwarmWorkerStandsByIdle(t *testing.T) {
	f := &leaderFake{
		nodeErr: errors.New("This node is not a swarm manager."),
		info:    runtime.Info{SwarmLocalNodeState: "active", SwarmControlAvailable: false},
	}
	var buf bytes.Buffer
	log := zerolog.New(&buf)

	ctx, cancel := context.WithCancel(context.Background())
	ch := WatchSwarmLeadership(ctx, f, log)

	// The loop must never start: no leader-true may arrive.
	select {
	case v := <-ch:
		t.Fatalf("swarm worker emitted %v, want silent idle (no control loop)", v)
	case <-time.After(500 * time.Millisecond):
	}

	cancel()
	// Drain until the channel closes so the goroutine has fully finished.
	for range ch {
	}

	if got := strings.Count(buf.String(), "this node is a swarm worker — standing by (no control loop)"); got != 1 {
		t.Errorf("standby message logged %d times, want exactly 1:\n%s", got, buf.String())
	}
}

// writeCtlplaneArchive drops a control-plane tarball into dir under the
// standard offen name so NewestControlPlaneArchive picks it up. The archive
// mirrors the offen layout: entries under backup/pmcluster/.
func writeCtlplaneArchive(t *testing.T, dir, name string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "backup/pmcluster", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	body := "sqlite-bytes"
	if err := tw.WriteHeader(&tar.Header{Name: "backup/pmcluster/data.db", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

// withArchiveDir points the archive scan at dir for the duration of the test.
func withArchiveDir(t *testing.T, dir string) {
	t.Helper()
	old := controlPlaneArchiveDir
	controlPlaneArchiveDir = dir
	t.Cleanup(func() { controlPlaneArchiveDir = old })
}

func TestEnsureControlPlaneFresh_KeepsCurrentDB(t *testing.T) {
	// Shared-storage (LINSTOR) case: data.db on this node is newer than the
	// newest archive → no restore, existing DB must remain untouched.
	archiveDir := t.TempDir()
	withArchiveDir(t, archiveDir)
	now := time.Now()
	writeCtlplaneArchive(t, archiveDir, "pmcluster-ctlplane-n1-2026-09-25T03-00-00.tar.gz", now.Add(-24*time.Hour))

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "data.db")
	if err := os.WriteFile(dbPath, []byte("live-db"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(dbPath, now, now) // DB newer than archive

	cfg := &config.Config{DataDir: dataDir}
	log := zerolog.Nop()
	restored, err := ensureControlPlaneFresh(context.Background(), cfg, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	if restored {
		t.Fatal("expected NO restore when local DB is current")
	}
	b, _ := os.ReadFile(dbPath)
	if string(b) != "live-db" {
		t.Fatal("current local DB was clobbered")
	}
}

func TestEnsureControlPlaneFresh_RestoresWhenStale(t *testing.T) {
	archiveDir := t.TempDir()
	withArchiveDir(t, archiveDir)
	now := time.Now()
	writeCtlplaneArchive(t, archiveDir, "pmcluster-ctlplane-n1-2026-09-25T03-00-00.tar.gz", now.Add(-2*time.Hour))

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "data.db")
	if err := os.WriteFile(dbPath, []byte("stale-db"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(dbPath, now.Add(-24*time.Hour), now.Add(-24*time.Hour)) // older than archive

	cfg := &config.Config{DataDir: dataDir}
	restored, err := ensureControlPlaneFresh(context.Background(), cfg, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("expected restore when local DB is older than newest archive")
	}
}

func TestEnsureControlPlaneFresh_RestoresWhenMissing(t *testing.T) {
	archiveDir := t.TempDir()
	withArchiveDir(t, archiveDir)
	now := time.Now()
	writeCtlplaneArchive(t, archiveDir, "pmcluster-ctlplane-n1-2026-09-25T03-00-00.tar.gz", now.Add(-2*time.Hour))

	// Empty data dir — no data.db at all. Expect restore.
	cfg := &config.Config{DataDir: t.TempDir()}
	restored, err := ensureControlPlaneFresh(context.Background(), cfg, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("expected restore when no local DB exists")
	}
}

func TestEnsureControlPlaneFresh_NoArchiveKeeps(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	restored, err := ensureControlPlaneFresh(context.Background(), cfg, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if restored {
		t.Fatal("expected no restore when no archive exists")
	}
}

// leaderlessDocker is a runtime.Client whose ConfigList fails with Docker's
// no-Raft-leader (quorum lost) error — the transient condition that used to
// make the daemon exit and systemd restart-loop it (BUG-020).
type leaderlessDocker struct{ runtime.Client }

func (leaderlessDocker) ConfigList(context.Context, string, string) ([]string, error) {
	return nil, errors.New("Error response from daemon: rpc error: code = Unknown desc = The swarm does not have a leader. It's possible that too few managers are online. Make sure more than half of the managers are online.")
}

func TestEnsureControlPlaneFresh_SwarmLeaderlessIsNonFatal(t *testing.T) {
	// Quorum lost while this daemon starts: the Raft kit is unreadable. The
	// daemon must keep serving (fall through to the tarball path, then the
	// local DB) instead of returning an error that exits the process —
	// otherwise every manager crash-loops for the duration of the outage
	// (BUG-020, reproduced live in TC10-A: 28 restarts on nxt-sw-4-m).
	withArchiveDir(t, t.TempDir()) // empty: no tarball to fall back to either
	cfg := &config.Config{DataDir: t.TempDir()}
	restored, err := ensureControlPlaneFresh(context.Background(), cfg, leaderlessDocker{}, zerolog.Nop())
	if err != nil {
		t.Fatalf("a leaderless swarm must not be fatal, got %v", err)
	}
	if restored {
		t.Fatal("no restore expected when the swarm is unavailable and no archive exists")
	}
}

// stateFake embeds runtime.Client and stores configs in memory so the
// Raft-replicated control-plane restore path runs without Docker.
type stateFake struct {
	runtime.Client
	configs map[string]runtime.ConfigInspectResult
}

func (f *stateFake) ConfigCreate(_ context.Context, spec runtime.ConfigSpec) error {
	f.configs[spec.Name] = runtime.ConfigInspectResult{Labels: spec.Labels, Data: spec.Data}
	return nil
}

func (f *stateFake) ConfigRemove(_ context.Context, name string) error {
	delete(f.configs, name)
	return nil
}

func (f *stateFake) ConfigList(_ context.Context, labelKey, labelValue string) ([]string, error) {
	var names []string
	for name, c := range f.configs {
		if labelKey == "" || c.Labels[labelKey] == labelValue {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (f *stateFake) ConfigInspect(_ context.Context, name string) (runtime.ConfigInspectResult, error) {
	c, ok := f.configs[name]
	if !ok {
		return runtime.ConfigInspectResult{}, fmt.Errorf("config %q not found", name)
	}
	return c, nil
}

// snapshotInto publishes a control-plane snapshot of srcDir into the fake.
func snapshotInto(t *testing.T, f *stateFake, srcDir string) {
	t.Helper()
	kit := &controlplane.Kit{Docker: f, DataDir: srcDir, Log: zerolog.Nop()}
	if err := kit.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureControlPlaneFresh_RestoresFromStateConfig(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "data.db"), []byte("raft-db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".encryption_key"), []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &stateFake{configs: map[string]runtime.ConfigInspectResult{}}
	snapshotInto(t, f, srcDir)

	cfg := &config.Config{DataDir: t.TempDir()}
	restored, err := ensureControlPlaneFresh(context.Background(), cfg, f, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("expected restore from the Raft state config")
	}
	if b, _ := os.ReadFile(cfg.DBPath()); string(b) != "raft-db" {
		t.Fatalf("data.db not restored from Raft config: %q", b)
	}
}

func TestEnsureControlPlaneFresh_KeepsCurrentStateConfig(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "data.db"), []byte("raft-db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".encryption_key"), []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &stateFake{configs: map[string]runtime.ConfigInspectResult{}}
	snapshotInto(t, f, srcDir)

	// Local control plane newer than the snapshot (shared storage): the Raft
	// config exists but nothing is restored, and the local DB survives.
	curDir := t.TempDir()
	dbPath := filepath.Join(curDir, "data.db")
	if err := os.WriteFile(dbPath, []byte("live-db"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(dbPath, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	if err := os.WriteFile(filepath.Join(curDir, ".encryption_key"), []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{DataDir: curDir}
	restored, err := ensureControlPlaneFresh(context.Background(), cfg, f, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if restored {
		t.Fatal("expected no restore when the local control plane is current")
	}
	if b, _ := os.ReadFile(dbPath); string(b) != "live-db" {
		t.Fatal("current local DB was clobbered")
	}
}

func TestEnsureControlPlaneFresh_MissingKeyConfigErrors(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "data.db"), []byte("raft-db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".encryption_key"), []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &stateFake{configs: map[string]runtime.ConfigInspectResult{}}
	snapshotInto(t, f, srcDir)
	// Break the security split: remove the key configs.
	for name := range f.configs {
		if strings.HasPrefix(name, "pmcluster_state_key_") {
			if err := f.ConfigRemove(context.Background(), name); err != nil {
				t.Fatal(err)
			}
		}
	}

	cfg := &config.Config{DataDir: t.TempDir()}
	if _, err := ensureControlPlaneFresh(context.Background(), cfg, f, zerolog.Nop()); err == nil {
		t.Fatal("restore must refuse a state config whose key config is missing")
	}
}

func TestEnsureControlPlaneFresh_FallsBackToTarballWhenNoStateConfig(t *testing.T) {
	// A live Docker client with NO state configs must fall back to the
	// tarball archive path (pre-L2 clusters).
	archiveDir := t.TempDir()
	withArchiveDir(t, archiveDir)
	now := time.Now()
	writeCtlplaneArchive(t, archiveDir, "pmcluster-ctlplane-n1-2026-09-25T03-00-00.tar.gz", now.Add(-2*time.Hour))

	f := &stateFake{configs: map[string]runtime.ConfigInspectResult{}}
	cfg := &config.Config{DataDir: t.TempDir()}
	restored, err := ensureControlPlaneFresh(context.Background(), cfg, f, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("expected tarball fallback when no Raft state config exists")
	}
}
