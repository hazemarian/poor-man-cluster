package noderuntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/controlplane"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
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
	leader, found, err := IsSwarmLeader(context.Background(), f, "mgr2")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected mgr2 to be found")
	}
	if !leader {
		t.Fatal("expected mgr2 to be leader")
	}
	notLeader, found, err := IsSwarmLeader(context.Background(), f, "mgr1")
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

// TestIsSwarmLeader_NilClientIsAnErrorNotAPanic is the regression for a daemon
// that died in CI: the telemetry supervisor's leader probe ran with a nil docker
// client (a malformed DOCKER_HOST was enough) and IsSwarmLeader dereferenced it, so
// a daemon that had deliberately degraded to "serve without docker" panicked on the
// first tick and /health never answered. A background panic is not a diagnosable
// failure — it is a missing health endpoint and a 15s waitHealthy timeout.
func TestIsSwarmLeader_NilClientIsAnErrorNotAPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("IsSwarmLeader panicked on a nil client: %v", r)
		}
	}()
	leader, found, err := IsSwarmLeader(context.Background(), nil, "node-1")
	if err == nil {
		t.Fatal("a nil docker client must be reported as an error, not as a leaderless node")
	}
	if leader || found {
		t.Fatalf("nil client answered leader=%v found=%v, want false/false", leader, found)
	}
}

func TestIsSwarmLeader_NodeNotFound(t *testing.T) {
	f := &leaderFake{nodes: []runtime.Node{
		{ID: "n1", Hostname: "mgr1", IsLeader: true},
	}}
	leader, found, err := IsSwarmLeader(context.Background(), f, "ghost")
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
	if err := WaitForSwarmLeadership(context.Background(), f, zerolog.Nop()); err != nil {
		t.Fatalf("expected immediate return on NodeList error, got %v", err)
	}
}

func TestWaitForSwarmLeadership_NodeNotInSwarmServesImmediately(t *testing.T) {
	// A host that is not a swarm member (no node with our hostname) must not
	// block — treat it as standalone/local.
	f := &leaderFake{nodes: []runtime.Node{{ID: "n1", Hostname: "someone-else", IsLeader: true}}}
	if err := WaitForSwarmLeadership(context.Background(), f, zerolog.Nop()); err != nil {
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
		done <- WaitForSwarmLeadership(ctx, f, zerolog.Nop())
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
	go func() { done <- WaitForSwarmLeadership(ctx, f, log) }()

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

// noConfigDocker is a runtime.Client that answers the config list with nothing: a swarm
// that has never published a control-plane key config. Every other method is the embedded
// nil interface, which is fine because nothing here calls one.
type noConfigDocker struct{ runtime.Client }

func (noConfigDocker) ConfigList(context.Context, string, string) ([]string, error) {
	return nil, nil
}

// confirmCfg is a config whose data dir optionally holds a store.
func confirmCfg(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{DataDir: dir}
}

// TestConfirmMirrorAtStartup_NoStoreSaysItIsWaiting is the startup rule after ADR 0005: the
// store is seeded by the mirror, so a manager with no data starts and says what it is doing.
// The assertion that it CREATES nothing is the important half — the old path copied a
// data.db onto the node, and a silent empty store would read as "the cluster is empty".
func TestConfirmMirrorAtStartup_NoStoreSaysItIsWaiting(t *testing.T) {
	cfg := confirmCfg(t)
	logs := &logCapture{}
	ConfirmMirrorAtStartup(context.Background(), cfg, logs.logger())

	entry := logs.find("waiting for the first snapshot")
	if entry == nil {
		t.Fatalf("no startup line said the node is waiting for a snapshot:\n%s", logs.text())
	}
	if entry.Level != "info" {
		t.Errorf("level = %q, want info — this is a new state, not a fault", entry.Level)
	}
	if _, err := os.Stat(cfg.DBPath()); !os.IsNotExist(err) {
		t.Errorf("ConfirmMirrorAtStartup created %s; nothing may seed the store but the mirror", cfg.DBPath())
	}
}

func TestConfirmMirrorAtStartup_NeverSynced(t *testing.T) {
	cfg := confirmCfg(t)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_ = st.Close()

	logs := &logCapture{}
	ConfirmMirrorAtStartup(context.Background(), cfg, logs.logger())
	if logs.find("never applied a snapshot") == nil {
		t.Fatalf("a store that has never mirrored was not reported as such:\n%s", logs.text())
	}
}

// TestConfirmMirrorAtStartup_ReportsTheMirror is the healthy case: the line names the leader,
// the version and the age, measured from checked_at (which moves on a 304) rather than from
// applied_at (which does not).
func TestConfirmMirrorAtStartup_ReportsTheMirror(t *testing.T) {
	cfg := confirmCfg(t)
	ctx := context.Background()
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	applied := time.Now().Add(-90 * time.Second)
	if err := st.ApplySnapshot(ctx, &store.Snapshot{
		Version: 12,
		Tables: map[string]store.TableData{
			"cluster_settings": {Columns: []string{"key", "value"}, Types: []string{"TEXT", "TEXT"}, Rows: [][]any{}},
		},
	}, "nxt-sw-5-m", applied.Unix()); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	if err := st.ConfirmMirrorSync(ctx, applied.Unix()); err != nil {
		t.Fatalf("ConfirmMirrorSync: %v", err)
	}
	_ = st.Close()

	logs := &logCapture{}
	ConfirmMirrorAtStartup(context.Background(), cfg, logs.logger())
	entry := logs.find("mirrored from the leader")
	if entry == nil {
		t.Fatalf("no startup line reported the mirror:\n%s", logs.text())
	}
	if !strings.Contains(string(entry.Fields["leader"]), "nxt-sw-5-m") {
		t.Errorf("the line does not name the leader: %s", entry.Message)
	}
	if !strings.Contains(string(entry.Fields["version"]), "12") {
		t.Errorf("the line does not carry the version: %s", entry.Fields["version"])
	}
}

// TestAdoptOnLeadershipGain_ServesAStaleMirrorAndSaysSo is the operator's ruling: a
// 30-minute-old mirror is SERVED (a promotion on old data beats no leader), the age is
// logged, and the adoption gate is left open.
func TestAdoptOnLeadershipGain_ServesAStaleMirrorAndSaysSo(t *testing.T) {
	ctx := context.Background()
	cfg := confirmCfg(t)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	old := time.Now().Add(-30 * time.Minute).Unix()
	if err := st.ApplySnapshot(ctx, &store.Snapshot{
		Version: 3,
		Tables: map[string]store.TableData{
			"cluster_settings": {Columns: []string{"key", "value"}, Types: []string{"TEXT", "TEXT"}, Rows: [][]any{}},
		},
	}, "old-leader", old); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	logs := &logCapture{}
	gate := &AdoptionGate{}
	kit := &controlplane.Kit{Docker: noConfigDocker{}, DataDir: cfg.DataDir, Log: logs.logger()}
	if ready := AdoptOnLeadershipGain(ctx, kit, st, gate, true, logs.logger()); !ready {
		t.Fatal("AdoptOnLeadershipGain refused a stale mirror, which the operator decided is served")
	}
	if gate.adopting() {
		t.Error("the adoption gate stayed closed after the check: every request but /health would answer 503 forever")
	}
	if logs.find("older than three polls") == nil {
		t.Fatalf("the stale adoption did not log its age and version:\n%s", logs.text())
	}
}

// TestAdoptOnLeadershipGain_EmptyStoreRefusesAndStartsNothing is the one hard refusal: no
// snapshot has ever been applied AND the store holds nothing, so there is nothing to serve.
// The caller gets false, so the control loop never starts.
func TestAdoptOnLeadershipGain_EmptyStoreRefusesAndStartsNothing(t *testing.T) {
	ctx := context.Background()
	cfg := confirmCfg(t)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	logs := &logCapture{}
	gate := &AdoptionGate{}
	kit := &controlplane.Kit{Docker: noConfigDocker{}, DataDir: cfg.DataDir, Log: logs.logger()}
	if ready := AdoptOnLeadershipGain(ctx, kit, st, gate, true, logs.logger()); ready {
		t.Fatal("AdoptOnLeadershipGain said ready for an empty store that has never mirrored")
	}
	if gate.adopting() {
		t.Error("the adoption gate stayed closed after a refusal")
	}
	if logs.find("refusing leadership") == nil {
		t.Fatalf("the refusal was not logged:\n%s", logs.text())
	}
}

// TestAdoptOnLeadershipGain_FreshMirrorIsReady is the promoted-manager case: the store is
// already the cluster's state, so it becomes the writer without copying anything.
func TestAdoptOnLeadershipGain_FreshMirrorIsReady(t *testing.T) {
	ctx := context.Background()
	cfg := confirmCfg(t)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.ApplySnapshot(ctx, &store.Snapshot{
		Version: 4,
		Tables: map[string]store.TableData{
			"cluster_settings": {Columns: []string{"key", "value"}, Types: []string{"TEXT", "TEXT"}, Rows: [][]any{}},
		},
	}, "nxt-sw-5-m", time.Now().Add(-10*time.Second).Unix()); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	gate := &AdoptionGate{}
	kit := &controlplane.Kit{Docker: noConfigDocker{}, DataDir: cfg.DataDir, Log: zerolog.Nop()}
	if ready := AdoptOnLeadershipGain(ctx, kit, st, gate, true, zerolog.Nop()); !ready {
		t.Fatal("AdoptOnLeadershipGain refused a mirror confirmed ten seconds ago")
	}
	if gate.adopting() {
		t.Error("the adoption gate stayed closed after a successful check")
	}
}
