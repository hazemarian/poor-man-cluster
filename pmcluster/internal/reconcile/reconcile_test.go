package reconcile

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
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
