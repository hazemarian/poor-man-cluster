package stacks

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestLastBackupJSON_NoneRecorded returns nil when the stack has no
// backup history yet.
func TestLastBackupJSON_NoneRecorded(t *testing.T) {
	st := newTestStore(t)
	got := lastBackupJSON(context.Background(), backups.NewLocal(st, nil), "no-such-stack")
	if got != nil {
		t.Errorf("expected nil for unknown stack, got %v", got)
	}
}

// TestLastBackupJSON_Succeeded returns the most-recent succeeded
// backup with status + timestamps + revision (no error_message).
func TestLastBackupJSON_Succeeded(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.CreateBackup(ctx, "my-stack", 1700000000)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if err := st.FinishBackup(ctx, id, "succeeded", "/var/backups/x.tar.gz", ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	got := lastBackupJSON(ctx, backups.NewLocal(st, nil), "my-stack")
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got["status"] != "succeeded" {
		t.Errorf("status: got %v, want \"succeeded\"", got["status"])
	}
	if got["revision"] != int64(1700000000) {
		t.Errorf("revision: got %v, want 1700000000", got["revision"])
	}
	if _, ok := got["error_message"]; ok {
		t.Errorf("error_message must be omitted on success, got %v", got["error_message"])
	}
	if _, ok := got["finished_at"]; !ok {
		t.Errorf("finished_at must be present on completed backup")
	}
}

// TestLastBackupJSON_Failed surfaces the error message so operators
// can see WHY the last backup failed without an extra round-trip.
func TestLastBackupJSON_Failed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.CreateBackup(ctx, "my-stack", 1700000001)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if err := st.FinishBackup(ctx, id, "failed", "", "offen exec exited 1"); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	got := lastBackupJSON(ctx, backups.NewLocal(st, nil), "my-stack")
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got["status"] != "failed" {
		t.Errorf("status: got %v, want \"failed\"", got["status"])
	}
	if got["error_message"] != "offen exec exited 1" {
		t.Errorf("error_message: got %v", got["error_message"])
	}
}

// TestLastBackupJSON_MostRecentFirst returns only the newest of
// multiple backups for the same stack.
func TestLastBackupJSON_MostRecentFirst(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	old, _ := st.CreateBackup(ctx, "my-stack", 1700000000)
	_ = st.FinishBackup(ctx, old, "succeeded", "", "")

	newer, _ := st.CreateBackup(ctx, "my-stack", 1700000099)
	_ = st.FinishBackup(ctx, newer, "failed", "", "boom")

	got := lastBackupJSON(ctx, backups.NewLocal(st, nil), "my-stack")
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got["status"] != "failed" {
		t.Errorf("expected newest (failed) backup, got status %v", got["status"])
	}
	if got["revision"] != int64(1700000099) {
		t.Errorf("expected newest revision 1700000099, got %v", got["revision"])
	}
}

// stubDeployer is a minimal cluster.StackDeployer for handler tests: it
// records which stacks were removed.
type stubDeployer struct {
	removed  []string
	deployed []string
}

func (s *stubDeployer) DeployStack(ctx context.Context, name string, _ []byte) error {
	s.deployed = append(s.deployed, name)
	return nil
}

func (s *stubDeployer) RemoveStack(_ context.Context, name string) error {
	s.removed = append(s.removed, name)
	return nil
}

func (s *stubDeployer) ForceUpdateService(context.Context, string) error { return nil }

func (s *stubDeployer) PruneStaleContainers(context.Context, string, string) error { return nil }

var _ cluster.StackDeployer = (*stubDeployer)(nil)

// TestRemoveStackHandler covers DELETE /api/stacks/{name}: a deployed stack is
// removed from the swarm and deleted from the store (200), and an unknown
// stack yields 404.
func TestRemoveStackHandler(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	rev := &store.StackRevision{
		StackName:    "demo",
		Revision:     1000,
		SourceYAML:   "app: demo",
		RenderedYAML: "services: {}",
	}
	if err := st.RecordDeploy(ctx, rev, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}

	dep := &stubDeployer{}
	svc := &Service{Store: st, Deployer: dep}
	h := &HTTP{Deploy: svc, Read: Local{Store: st}}

	r := chi.NewRouter()
	h.Mount(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/stacks/demo", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /stacks/demo = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(dep.removed) != 1 || dep.removed[0] != "demo" {
		t.Errorf("RemoveStack calls = %v, want [demo]", dep.removed)
	}
	if _, err := st.GetStack(ctx, "demo"); !errors.Is(err, store.ErrStackNotFound) {
		t.Errorf("stack row still present after DELETE: %v", err)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/stacks/ghost", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE /stacks/ghost = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
}

// TestSyncStackHandler verifies POST /stacks/{name}/sync re-runs the deploy
// pipeline from the latest stored manifest and records a new revision.
func TestSyncStackHandler(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	rev := &store.StackRevision{
		StackName:    "demo",
		Revision:     1000,
		SourceYAML:   "app: demo\nenv: production\ndomain: example.test\nservices:\n  web:\n    image: nginx\n",
		RenderedYAML: "services:\n  web:\n    image: nginx\n",
	}
	if err := st.RecordDeploy(ctx, rev, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}

	dep := &stubDeployer{}
	svc := &Service{Store: st, Deployer: dep}
	h := &HTTP{Deploy: svc, Read: Local{Store: st}}

	r := chi.NewRouter()
	h.Mount(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stacks/demo/sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /stacks/demo/sync = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(dep.deployed) == 0 {
		t.Fatal("sync did not re-deploy the stack")
	}
	stk, err := st.GetStack(ctx, "demo")
	if err != nil {
		t.Fatalf("GetStack: %v", err)
	}
	if stk.CurrentRevision <= 1000 {
		t.Errorf("CurrentRevision = %d, want > 1000 (new revision recorded)", stk.CurrentRevision)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stacks/ghost/sync", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /stacks/ghost/sync = %d, want 400 (no revisions); body: %s", rec.Code, rec.Body.String())
	}
}
