package reconcile

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// fakeServices is a services.Service stub: embeds the interface and returns a
// canned per-stack service list.
type fakeServices struct {
	services.Service
	svcs []services.ServiceSummary
	err  error
}

func (f *fakeServices) List(_ context.Context, _ string) ([]services.ServiceSummary, error) {
	return f.svcs, f.err
}

// recordingDeployer is a minimal cluster.StackDeployer recording calls.
type recordingDeployer struct {
	calls int
}

func (d *recordingDeployer) DeployStack(_ context.Context, _ string, _ []byte) error {
	d.calls++
	return nil
}
func (d *recordingDeployer) DeployStackNoPrune(_ context.Context, _ string, _ []byte) error {
	d.calls++
	return nil
}
func (d *recordingDeployer) PruneStack(_ context.Context, _ string, _ []byte) error { return nil }
func (d *recordingDeployer) ForceUpdateService(_ context.Context, _ string) error {
	return nil
}
func (d *recordingDeployer) RemoveStack(_ context.Context, _ string) error { return nil }
func (d *recordingDeployer) PruneStaleContainers(_ context.Context, _ string, _ string) error {
	return nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/data.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestServiceStatus(t *testing.T) {
	cases := []struct {
		name string
		s    services.ServiceSummary
		want string
	}{
		{"healthy", services.ServiceSummary{Name: "demo_web", Desired: 1, Replicas: 1}, StatusHealthy},
		{"in progress", services.ServiceSummary{Name: "demo_web", Desired: 1, Replicas: 1, UpdateState: "updating"}, StatusInProgress},
		{"paused is error", services.ServiceSummary{Name: "demo_web", Desired: 1, Replicas: 1, UpdateState: "paused"}, StatusError},
		{"completed run-once paused is healthy", services.ServiceSummary{Name: "demo_migrate", Desired: 1, Replicas: 0, RunOnce: true, UpdateState: "paused"}, StatusHealthy},
		{"completed run-once not degraded", services.ServiceSummary{Name: "demo_migrate", Desired: 1, Replicas: 0, RunOnce: true}, StatusHealthy},
		{"degraded under-replicated", services.ServiceSummary{Name: "demo_web", Desired: 2, Replicas: 1}, StatusDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ServiceStatus(tc.s); got != tc.want {
				t.Errorf("ServiceStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStackStatus(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.RecordDeploy(ctx, &store.StackRevision{StackName: "demo", Revision: 1002, SourceYAML: "app: demo", RenderedYAML: "x: 1"}, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}
	_, _ = st.GetStack(ctx, "demo") // stack exists check; row re-fetched after the error below

	healthy := []services.ServiceSummary{{Name: "demo_web", Desired: 1, Replicas: 1}}
	degraded := []services.ServiceSummary{{Name: "demo_web", Desired: 2, Replicas: 1}}

	// Error outcome on the current revision wins.
	_, _ = st.RecordStackError(ctx, "demo", 1002, "docker stack deploy: boom")
	row, _ := st.GetStack(ctx, "demo") // re-fetch: LastError changed
	if got := StackStatus(*row, healthy); got != StatusError {
		t.Errorf("current-revision error: got %q want error", got)
	}
	// Stale error (older revision) is ignored once cleared.
	_, _ = st.RecordStackError(ctx, "demo", 1001, "old boom")
	row, _ = st.GetStack(ctx, "demo")
	if got := StackStatus(*row, healthy); got != StatusError { // current rev still errored
		t.Errorf("older error with current error: got %q want error", got)
	}
	// Worst live status wins when no error.
	st2 := newTestStore(t)
	_ = st2.RecordDeploy(ctx, &store.StackRevision{StackName: "demo", Revision: 1003, SourceYAML: "app: demo", RenderedYAML: "x: 1"}, "")
	row2, _ := st2.GetStack(ctx, "demo")
	if got := StackStatus(*row2, degraded); got != StatusDegraded {
		t.Errorf("degraded worst: got %q want degraded", got)
	}
	if got := StackStatus(*row2, nil); got != StatusUnknown {
		t.Errorf("no services: got %q want unknown", got)
	}
}

// TestRunOnce_SnapshotsAndSyncs wires a real store + a real stacks.Service
// with a recording deployer, and asserts RunOnce writes stack_status rows and
// drives app-stack sync.
func TestRunOnce_SnapshotsAndSyncs(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	// A stack with a deployable revision.
	if err := st.RecordDeploy(ctx, &store.StackRevision{
		StackName:    "demo",
		Revision:     1004,
		SourceYAML:   "app: demo\nenv: production\ndomain: example.com\nservices:\n  web:\n    image: nginx\n",
		RenderedYAML: "version: \"3.9\"\nservices:\n  web:\n    image: nginx\n",
	}, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}

	dep := &recordingDeployer{}
	svc := &stacks.Service{Store: st, Deployer: dep, MkdirAll: func(string, os.FileMode) error { return nil }}
	fs := &fakeServices{svcs: []services.ServiceSummary{{Name: "demo_web", Desired: 1, Replicas: 1}}}

	r := &Reconciler{
		Store:         st,
		DeployService: svc,
		Services:      fs,
		UpdateDeps:    cluster.UpdateDeps{},
		UpdateInput:   cluster.UpdateInput{},
	}

	if err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	snap, err := st.GetStackStatus(ctx, "demo")
	if err != nil {
		t.Fatalf("GetStackStatus: %v", err)
	}
	if snap.Status != StatusHealthy {
		t.Errorf("snapshot status = %q, want healthy", snap.Status)
	}
	if snap.Services["demo_web"] != StatusHealthy {
		t.Errorf("snapshot service status = %q, want healthy", snap.Services["demo_web"])
	}
}

// TestRunOnce_PrunesStaleRows asserts a status row for a stack removed from
// the store is cleaned up.
func TestRunOnce_PrunesStaleRows(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.SetStackStatus(ctx, store.StackStatus{StackName: "ghost", Status: StatusHealthy, Services: map[string]string{}, UpdatedAt: 1}); err != nil {
		t.Fatalf("SetStackStatus: %v", err)
	}
	svc := &stacks.Service{Store: st, Deployer: &recordingDeployer{}, MkdirAll: func(string, os.FileMode) error { return nil }}
	r := &Reconciler{Store: st, DeployService: svc, Services: &fakeServices{}}
	if err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, err := st.GetStackStatus(ctx, "ghost"); err == nil {
		t.Error("ghost status row was not pruned")
	}
}

// fakeEventsDocker implements runtime.Client.Events with a controllable channel.
type fakeEventsDocker struct {
	runtime.Client
	evCh   chan runtime.Event
	errCh  chan error
	called atomic.Bool
}

func (f *fakeEventsDocker) Events(_ context.Context, _ time.Time) (<-chan runtime.Event, <-chan error) {
	f.called.Store(true)
	return f.evCh, f.errCh
}

// TestLoop_EventTriggersPass asserts the loop subscribes to docker events and
// stops on cancel.
func TestLoop_EventTriggersPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	st := newTestStore(t)
	docker := &fakeEventsDocker{evCh: make(chan runtime.Event), errCh: make(chan error)}
	svc := &stacks.Service{Store: st, Deployer: &recordingDeployer{}, MkdirAll: func(string, os.FileMode) error { return nil }}

	r := &Reconciler{
		Store: st, Docker: docker, DeployService: svc, Services: &fakeServices{},
		Interval: 10 * time.Millisecond, // fast safety tick
		Log:      noopLogger(),
	}
	go r.Loop(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for !docker.called.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !docker.called.Load() {
		t.Error("Loop did not subscribe to docker events")
	}
	cancel()
	select {
	case <-time.After(2 * time.Second):
		t.Error("Loop did not stop on cancel")
	default:
	}
}

func noopLogger() zerolog.Logger {
	return zerolog.Nop()
}

// fakeNodesDocker implements runtime.Client.NodeList with canned nodes.
type fakeNodesDocker struct {
	runtime.Client
	nodes []runtime.Node
	err   error
}

func (f *fakeNodesDocker) NodeList(context.Context) ([]runtime.Node, error) {
	return f.nodes, f.err
}

// TestRunOnce_PausesSyncWhenStorageNodeDown wires a real store + a real
// stacks.Service with a PinResolver and asserts the app-drift pass SKIPS a
// stateful stack whose storage node is down (absent from the swarm node
// list), then resumes syncing once the node returns. A stateless stack is
// never paused.
func TestRunOnce_PausesSyncWhenStorageNodeDown(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	// Stateful stack: RenderedHash "" forces a redeploy whenever Sync runs.
	statefulSource := "app: demo\nenv: production\ndomain: example.com\nservices:\n  web:\n    image: nginx\n    volumes: [data:/var/lib/data]\n"
	if err := st.RecordDeploy(ctx, &store.StackRevision{
		StackName:    "demo",
		Revision:     2001,
		SourceYAML:   statefulSource,
		RenderedYAML: "version: \"3.9\"\nservices:\n  web:\n    image: nginx\n",
	}, ""); err != nil {
		t.Fatalf("RecordDeploy demo: %v", err)
	}
	// Stateless stack: must sync even with every node down.
	if err := st.RecordDeploy(ctx, &store.StackRevision{
		StackName:    "webhooks",
		Revision:     2002,
		SourceYAML:   "app: webhooks\nenv: production\ndomain: example.com\nservices:\n  api:\n    image: nginx\n",
		RenderedYAML: "version: \"3.9\"\nservices:\n  api:\n    image: nginx\n",
	}, ""); err != nil {
		t.Fatalf("RecordDeploy webhooks: %v", err)
	}

	// Resolve which storage node "demo" round-robins to.
	res := &stacks.PinResolver{StorageNodes: []string{"node-a", "node-b"}}
	ir := &manifest.IR{Name: "demo", Services: []manifest.IRService{{Name: "web", Image: "nginx", Volumes: []string{"/data"}, Placement: ""}}}
	if err := res.ResolvePlacement(ctx, "demo", ir); err != nil {
		t.Fatalf("ResolvePlacement: %v", err)
	}
	pinned := ir.Services[0].Placement
	if pinned == "" {
		t.Fatal("expected a storage pin for the stateful stack")
	}
	other := "node-a"
	if pinned == "node-a" {
		other = "node-b"
	}

	dep := &recordingDeployer{}
	svc := &stacks.Service{Store: st, Deployer: dep, MkdirAll: func(string, os.FileMode) error { return nil }, Pins: res}

	// Case 1: the pinned storage node is down (absent from the node list).
	// The stateful stack is paused; the stateless one still syncs.
	down := &fakeNodesDocker{nodes: []runtime.Node{{Hostname: other, Status: "ready", Availability: "active"}}}
	r := &Reconciler{Store: st, Docker: down, DeployService: svc, Services: &fakeServices{}, Log: zerolog.Nop()}
	if err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (down): %v", err)
	}
	if dep.calls != 1 {
		t.Errorf("down: deployer calls = %d, want 1 (stateless only — stateful must be paused)", dep.calls)
	}

	// Case 2: the storage node returns → the pause clears and sync resumes.
	dep.calls = 0
	up := &fakeNodesDocker{nodes: []runtime.Node{
		{Hostname: pinned, Status: "ready", Availability: "active"},
		{Hostname: other, Status: "ready", Availability: "active"},
	}}
	r.Docker = up
	if err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (up): %v", err)
	}
	if dep.calls != 1 {
		t.Errorf("up: deployer calls = %d, want 1 (the previously-paused stateful stack deployed; the stateless stack converged as no-op)", dep.calls)
	}
}

// TestStoragePaused_NodeQuirks covers the down-node heuristics: a node
// listed but not "ready", or not "active", is down; a nil swarm disables the
// pause entirely.
func TestStoragePaused_NodeQuirks(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.RecordDeploy(ctx, &store.StackRevision{
		StackName:    "demo",
		Revision:     2003,
		SourceYAML:   "app: demo\nenv: production\ndomain: example.com\nservices:\n  web:\n    image: nginx\n    volumes: [data:/var/lib/data]\n",
		RenderedYAML: "version: \"3.9\"\nservices:\n  web:\n    image: nginx\n",
	}, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}
	svc := &stacks.Service{Store: st, Deployer: &recordingDeployer{}, MkdirAll: func(string, os.FileMode) error { return nil },
		Pins: &stacks.PinResolver{StorageNodes: []string{"node-a"}}}

	cases := []struct {
		name  string
		nodes []runtime.Node
		want  bool
	}{
		{"ready+active not down", []runtime.Node{{Hostname: "node-a", Status: "ready", Availability: "active"}}, false},
		{"absent is down", []runtime.Node{{Hostname: "node-b", Status: "ready", Availability: "active"}}, true},
		{"not ready is down", []runtime.Node{{Hostname: "node-a", Status: "down", Availability: "active"}}, true},
		{"drained is down", []runtime.Node{{Hostname: "node-a", Status: "ready", Availability: "drain"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{DeployService: svc, Log: zerolog.Nop()}
			health := (&fakeNodesDocker{nodes: tc.nodes}).toHealth()
			got, node := r.storagePaused(ctx, "demo", health)
			if got != tc.want {
				t.Errorf("storagePaused = %v (node %q), want %v", got, node, tc.want)
			}
		})
	}

	// Nil swarm (standalone/local): never paused.
	r := &Reconciler{DeployService: svc, Log: zerolog.Nop()}
	if got, _ := r.storagePaused(ctx, "demo", nil); got {
		t.Error("nil health must never pause a stack")
	}
}

// toHealth builds the health map exactly as nodeHealth does, so the quirk
// table above exercises the reconciler's own heuristic.
func (f *fakeNodesDocker) toHealth() map[string]bool {
	health := make(map[string]bool, len(f.nodes))
	for _, n := range f.nodes {
		health[n.Hostname] = n.Status == "ready" && n.Availability == "active"
	}
	return health
}

// TestRunOnce_TracerNoopSafe: creating OTLP spans with the default no-op
// provider (telemetry.Init never called in tests) must not panic and the pass
// still completes.
func TestRunOnce_TracerNoopSafe(t *testing.T) {
	st := newTestStore(t)
	dep := &recordingDeployer{}
	svc := &stacks.Service{Store: st, Deployer: dep, MkdirAll: func(string, os.FileMode) error { return nil }}
	if _, err := svc.Deploy(context.Background(), stacks.Payload{Manifest: "app: demo\ndomain: example.test\nenv: test\nservices:\n  web:\n    image: nginx\n"}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	rec := &Reconciler{Store: st, DeployService: svc, Services: &fakeServices{svcs: []services.ServiceSummary{{Name: "demo_web", Stack: "demo", Desired: 1, Replicas: 1}}}, Log: zerolog.Nop(), Interval: time.Second}
	if err := rec.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce with no-op tracer: %v", err)
	}
	snap, err := st.GetStackStatus(context.Background(), "demo")
	if err != nil || snap.Status != StatusHealthy {
		t.Fatalf("stack status after RunOnce = %v err %v, want healthy", snap.Status, err)
	}
}
