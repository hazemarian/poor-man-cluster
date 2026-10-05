package stacks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
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

func (s *stubDeployer) DeployStackNoPrune(ctx context.Context, name string, _ []byte) error {
	s.deployed = append(s.deployed, name)
	return nil
}

func (s *stubDeployer) PruneStack(context.Context, string, []byte) error { return nil }

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

// TestRevisionErrorJoin verifies each revision's outcome is JOINED from the
// stack's error history (stacks.last_error JSON array) — no per-revision
// storage. The list endpoint and the revision endpoint both expose the error.
func TestRevisionErrorJoin(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	rev1 := &store.StackRevision{StackName: "demo", Revision: 1001, SourceYAML: "app: demo", RenderedYAML: "services: {}"}
	if err := st.RecordDeploy(ctx, rev1, ""); err != nil {
		t.Fatalf("RecordDeploy v1: %v", err)
	}
	rev2 := &store.StackRevision{StackName: "demo", Revision: 1002, SourceYAML: "app: demo", RenderedYAML: "services: {}"}
	if err := st.RecordDeploy(ctx, rev2, ""); err != nil {
		t.Fatalf("RecordDeploy v2: %v", err)
	}
	// A failed execution and a clean one. Only failures are recorded — the
	// clean revision gets "" by absence from the history.
	if _, err := st.RecordStackError(ctx, "demo", 1002, "docker stack deploy: boom"); err != nil {
		t.Fatalf("RecordStackError 1002: %v", err)
	}

	dep := &stubDeployer{}
	svc := &Service{Store: st, Deployer: dep}
	h := &HTTP{Deploy: svc, Read: Local{Store: st}}

	r := chi.NewRouter()
	h.Mount(r)

	// List endpoint joins per-revision errors.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stacks/demo", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /stacks/demo = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Revisions []struct {
			Revision int64  `json:"revision"`
			Error    string `json:"error"`
		} `json:"revisions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(out.Revisions) != 2 {
		t.Fatalf("revisions = %d, want 2", len(out.Revisions))
	}
	byRev := map[int64]string{}
	for _, rv := range out.Revisions {
		byRev[rv.Revision] = rv.Error
	}
	if byRev[1002] != "docker stack deploy: boom" {
		t.Errorf("rev 1002 error = %q, want the joined failure", byRev[1002])
	}
	if byRev[1001] != "" {
		t.Errorf("rev 1001 error = %q, want empty (clean execution)", byRev[1001])
	}

	// Revision detail endpoint also joins the error.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stacks/demo/revisions/1002", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /stacks/demo/revisions/1002 = %d; body: %s", rec.Code, rec.Body.String())
	}
	var revOut struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &revOut); err != nil {
		t.Fatalf("unmarshal revision: %v", err)
	}
	if revOut.Error != "docker stack deploy: boom" {
		t.Errorf("revision detail error = %q, want the joined failure", revOut.Error)
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

// TestSyncStackHandler_NoOp verifies POST /stacks/{name}/sync returns
// changed:false and does not re-deploy when re-translation matches the stored
// rendered_hash.
func TestSyncStackHandler_NoOp(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	const manifest = "app: demo\nenv: production\ndomain: example.test\nservices:\n  web:\n    image: nginx\n"

	dep := &stubDeployer{}
	svc := &Service{Store: st, Deployer: dep}

	// Deploy once to establish the real rendered output + hash baseline.
	res, err := svc.Deploy(ctx, Payload{AppName: "demo", Manifest: manifest})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(dep.deployed) != 1 {
		t.Fatalf("deployer received %d calls, want 1", len(dep.deployed))
	}

	h := &HTTP{Deploy: svc, Read: Local{Store: st}}
	r := chi.NewRouter()
	h.Mount(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stacks/demo/sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /stacks/demo/sync = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(dep.deployed) != 1 {
		t.Errorf("sync re-deployed on unchanged rendered content: %v", dep.deployed)
	}
	if !strings.Contains(rec.Body.String(), `"changed":false`) {
		t.Errorf("response body = %s, want changed:false", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), fmt.Sprintf(`"revision":%d`, res.Revision)) {
		t.Errorf("response body = %s, want original revision %d preserved", rec.Body.String(), res.Revision)
	}
}

// TestAckStackHandler covers POST /stacks/{name}/ack: acknowledging an open
// storage-failover marker returns 200 + acknowledged:true (and flips the
// marker the read side exposes), while a stack with no open marker answers
// 404 — there is nothing to acknowledge.
func TestAckStackHandler(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.RecordDeploy(ctx, &store.StackRevision{
		StackName:    "demo",
		Revision:     1000,
		SourceYAML:   "app: demo",
		RenderedYAML: "services: {}",
	}, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}
	if err := st.SetStackFailover(ctx, store.StackFailover{
		StackName: "demo", FromNode: "node-a", ToNode: "node-b", At: 1700000001,
	}); err != nil {
		t.Fatalf("SetStackFailover: %v", err)
	}

	svc := &Service{Store: st, Deployer: &stubDeployer{}}
	h := &HTTP{Deploy: svc, Read: Local{Store: st}}
	r := chi.NewRouter()
	h.Mount(r)

	// The read side carries the open marker while it is unacknowledged.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stacks/demo", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /stacks/demo = %d; body: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"Acked":false`) || !strings.Contains(body, "node-a") {
		t.Errorf("GET /stacks/demo must expose the open failover marker, body: %s", body)
	}

	// Ack → 200, acknowledged:true, marker flipped.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stacks/demo/ack", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /stacks/demo/ack = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"acknowledged":true`) || !strings.Contains(body, `"demo"`) {
		t.Errorf("ack body = %s, want stack demo acknowledged", body)
	}
	fo, err := st.GetStackFailover(ctx, "demo")
	if err != nil {
		t.Fatalf("GetStackFailover after ack: %v", err)
	}
	if !fo.Acked {
		t.Error("marker not acknowledged after POST /stacks/demo/ack")
	}

	// No marker anywhere → 404 (nothing to acknowledge).
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stacks/ghost/ack", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /stacks/ghost/ack = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "no open failover marker") {
		t.Errorf("404 body = %s, want the no-open-marker message", body)
	}

	// Marker cleared (moved back) → the same 404 on a retry.
	if err := st.ClearStackFailover(ctx, "demo"); err != nil {
		t.Fatalf("ClearStackFailover: %v", err)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stacks/demo/ack", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /stacks/demo/ack after clear = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
}

// TestDeployConflictRejectedAtAPI verifies the app-name duplication guard
// surfaces through the HTTP surface: a second deploy from a different repo
// yields 400 with the conflict message, and nothing is deployed or recorded.
func TestDeployConflictRejectedAtAPI(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	manifest := "app: demo\nenv: production\ndomain: example.test\nservices:\n  web:\n    image: nginx\n"

	dep := &stubDeployer{}
	svc := &Service{Store: st, Deployer: dep, MkdirAll: func(string, os.FileMode) error { return nil }}
	h := &HTTP{Deploy: svc, Read: Local{Store: st}}

	r := chi.NewRouter()
	h.Mount(r)

	first := `{"app_name":"demo","repo_url":"https://github.com/org/one","file":"deploy/a.yaml","manifest":` +
		fmt.Sprintf("%q", manifest) + `}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stacks", strings.NewReader(first)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first deploy = %d, want 202 (fire-and-forget); body: %s", rec.Code, rec.Body.String())
	}

	conflict := `{"app_name":"demo","repo_url":"https://github.com/other/two","file":"deploy/b.yaml","manifest":` +
		fmt.Sprintf("%q", manifest) + `}`
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stacks", strings.NewReader(conflict)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("conflicting deploy = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already exists from repo") {
		t.Errorf("body = %s, want 'already exists from repo'", rec.Body.String())
	}

	stk, err := st.GetStack(ctx, "demo")
	if err != nil {
		t.Fatalf("GetStack: %v", err)
	}
	if stk.CurrentRevision == 0 {
		t.Error("stack missing after conflicting deploy")
	}
}
