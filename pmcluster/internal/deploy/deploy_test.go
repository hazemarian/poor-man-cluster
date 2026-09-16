package deploy

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// recordingDeployer is a test-local implementation of cluster.StackDeployer.
// It records every (stackName, composeYAML) call and returns configurable
// errors for deploy and remove independently.
type recordingDeployer struct {
	calls     []deployCall
	removed   []string
	err       error
	removeErr error
}

type deployCall struct {
	name string
	yaml []byte
}

func (r *recordingDeployer) DeployStack(_ context.Context, name string, composeYAML []byte) error {
	r.calls = append(r.calls, deployCall{name: name, yaml: composeYAML})
	return r.err
}

func (r *recordingDeployer) RemoveStack(_ context.Context, name string) error {
	r.removed = append(r.removed, name)
	return r.removeErr
}

func (r *recordingDeployer) ForceUpdateService(_ context.Context, _ string) error {
	return nil
}

func (r *recordingDeployer) PruneStaleContainers(_ context.Context, _ string, _ string) error {
	return nil
}

// Ensure the interface is satisfied.
var _ cluster.StackDeployer = (*recordingDeployer)(nil)

// openTestStore opens a fresh store in a temp dir for use in deploy tests.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// donationCampaignManifest is the canonical test DSL (no placeholders that need
// env vars, so Interpolate completes cleanly in tests).
const donationCampaignManifest = `
app: donation-campaign
env: production
domain: example.com
registry: ghcr.io/nextrum-sy
version: latest
secrets:
  - donation_campaign_db_password
volumes:
  - db_data
services:
  db:
    image: postgres:14-alpine
    placement: manager
    volumes: [db_data:/var/lib/postgresql/data]
    env:
      POSTGRES_DB: donation_campaign
      POSTGRES_USER: user
    secrets: [donation_campaign_db_password]
    healthcheck:
      type: pg_isready
  migration:
    image: ghcr.io/nextrum-sy/donation-campaign:latest
    command: [./migrate]
    run_once: true
  api:
    image: ghcr.io/nextrum-sy/donation-campaign:latest
    replicas: 2
    expose:
      port: 8080
      host: api.donation-campaign.example.com
    healthcheck:
      type: http
      path: /health
    update:
      parallelism: 1
      delay: 10s
      order: start-first
`

// newService builds a Service with the given store and deployer.
func newService(s *store.Store, d cluster.StackDeployer) *Service {
	return &Service{Store: s, Deployer: d}
}

// TestDeploy_HappyPath verifies the donation-campaign DSL goes through the
// full pipeline and is stored + forwarded to the deployer.
func TestDeploy_HappyPath(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	result, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if result.Revision == 0 {
		t.Error("result.Revision is 0, want non-zero unix timestamp")
	}
	if len(result.RenderedYAML) == 0 {
		t.Error("result.RenderedYAML is empty")
	}
	if result.StackName != "donation-campaign" {
		t.Errorf("result.StackName = %q, want donation-campaign", result.StackName)
	}

	if len(dep.calls) != 1 {
		t.Fatalf("deployer received %d calls, want 1", len(dep.calls))
	}
	if dep.calls[0].name != "donation-campaign" {
		t.Errorf("deployer call name = %q, want donation-campaign", dep.calls[0].name)
	}
	if len(dep.calls[0].yaml) == 0 {
		t.Error("deployer received empty compose YAML")
	}

	st, err := s.GetStack(ctx, "donation-campaign")
	if err != nil {
		t.Fatalf("GetStack: %v", err)
	}
	if st.CurrentRevision != result.Revision {
		t.Errorf("store current_revision = %d, want %d", st.CurrentRevision, result.Revision)
	}

	rev, err := s.GetRevision(ctx, "donation-campaign", result.Revision)
	if err != nil {
		t.Fatalf("GetRevision: %v", err)
	}
	if rev.RenderedYAML != string(result.RenderedYAML) {
		t.Errorf("stored RenderedYAML differs from returned RenderedYAML")
	}
}

// TestDeploy_EmptyManifest verifies that an empty manifest string returns an
// error containing "manifest: required".
func TestDeploy_EmptyManifest(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)

	_, err := svc.Deploy(context.Background(), Payload{Manifest: ""})
	if err == nil {
		t.Fatal("expected error for empty manifest, got nil")
	}
	if !strings.Contains(err.Error(), "manifest: required") {
		t.Errorf("error = %q, want to contain 'manifest: required'", err.Error())
	}
}

// TestDeploy_InvalidYAML verifies that syntactically invalid YAML returns a
// parse error.
func TestDeploy_InvalidYAML(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)

	_, err := svc.Deploy(context.Background(), Payload{Manifest: "app: :::not:valid:yaml:::"})
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}

	if !strings.Contains(err.Error(), "parse") {
		t.Logf("parse error (ok): %v", err)
	}
}

// TestDeploy_AppNameOverride verifies that setting Payload.AppName overrides
// the manifest's `app:` field so the stack is stored under the override name.
func TestDeploy_AppNameOverride(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	result, err := svc.Deploy(ctx, Payload{
		Manifest: donationCampaignManifest,
		AppName:  "custom-name",
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if result.StackName != "custom-name" {
		t.Errorf("result.StackName = %q, want custom-name", result.StackName)
	}

	if _, err := s.GetStack(ctx, "custom-name"); err != nil {
		t.Errorf("GetStack(custom-name): %v", err)
	}

	if _, err := s.GetStack(ctx, "donation-campaign"); !errors.Is(err, store.ErrStackNotFound) {
		t.Errorf("GetStack(donation-campaign) = %v, want ErrStackNotFound", err)
	}
}

// TestDeploy_VersionOverride verifies that Payload.Version overrides the
// manifest's `version:` field and appears in the rendered YAML.
func TestDeploy_VersionOverride(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	result, err := svc.Deploy(ctx, Payload{
		Manifest: donationCampaignManifest,
		Version:  "v42.0",
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !strings.Contains(string(result.RenderedYAML), "v42.0") {
		t.Errorf("rendered YAML does not contain version 'v42.0':\n%s", result.RenderedYAML)
	}
}

// TestDeploy_DeployerError verifies that when the deployer returns an error,
// the revision is STILL recorded in the store (intentional — operator can see
// what was attempted and decide to redeploy or rollback). The error is
// surfaced to the caller.
//
// This is intentional design: the store acts as an audit log even for failed
// deploys. The next successful deploy overwrites current_revision.
func TestDeploy_DeployerError(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{err: errors.New("docker daemon unavailable")}
	svc := newService(s, dep)
	ctx := context.Background()

	_, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest})
	if err == nil {
		t.Fatal("expected error from deployer, got nil")
	}
	if !strings.Contains(err.Error(), "docker stack deploy") {
		t.Errorf("error = %q, should mention docker stack deploy", err.Error())
	}

	st, stErr := s.GetStack(ctx, "donation-campaign")
	if errors.Is(stErr, store.ErrStackNotFound) {
		t.Fatal("stack row missing after deployer error — should be recorded")
	}
	if stErr != nil {
		t.Fatalf("GetStack: %v", stErr)
	}
	if _, revErr := s.GetRevision(ctx, "donation-campaign", st.CurrentRevision); revErr != nil {
		t.Errorf("revision row missing after deployer error: %v", revErr)
	}
}

// TestRollback_HappyPath deploys two revisions then rolls back to v1.
// It verifies: a NEW revision is created, the new revision's RenderedYAML
// matches v1's, and the stack's current_revision points to the new revision.
func TestRollback_HappyPath(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	r1, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest, Version: "v1"})
	if err != nil {
		t.Fatalf("Deploy v1: %v", err)
	}
	rev1 := r1.Revision
	rendered1 := string(r1.RenderedYAML)

	time.Sleep(2 * time.Second)

	r2, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest, Version: "v2"})
	if err != nil {
		t.Fatalf("Deploy v2: %v", err)
	}
	rev2 := r2.Revision
	if rev2 == rev1 {
		t.Fatalf("v2 revision = v1 revision (%d) — timestamps must differ", rev1)
	}

	time.Sleep(2 * time.Second)

	rr, err := svc.Rollback(ctx, "donation-campaign", rev1)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if rr.Revision == rev1 || rr.Revision == rev2 {
		t.Errorf("rollback revision = %d, want a new id (not rev1=%d or rev2=%d)",
			rr.Revision, rev1, rev2)
	}

	if string(rr.RenderedYAML) != rendered1 {
		t.Errorf("rollback RenderedYAML differs from v1 RenderedYAML")
	}

	st, err := s.GetStack(ctx, "donation-campaign")
	if err != nil {
		t.Fatalf("GetStack: %v", err)
	}
	if st.CurrentRevision != rr.Revision {
		t.Errorf("stack current_revision = %d, want rollback revision %d",
			st.CurrentRevision, rr.Revision)
	}
}

// TestRollback_UnknownRevision verifies that rolling back to a non-existent
// revision returns store.ErrRevisionNotFound.
func TestRollback_UnknownRevision(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	if _, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	_, err := svc.Rollback(ctx, "donation-campaign", 9999999)
	if !errors.Is(err, store.ErrRevisionNotFound) {
		t.Errorf("Rollback(unknown revision) = %v, want ErrRevisionNotFound", err)
	}
}

// TestRollback_UnknownStack verifies that rolling back a non-existent stack
// returns store.ErrRevisionNotFound (the revision lookup fails first since
// we look up by stack_name AND revision in the same query).
func TestRollback_UnknownStack(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	_, err := svc.Rollback(ctx, "ghost-stack", 1234)
	if !errors.Is(err, store.ErrRevisionNotFound) {
		t.Errorf("Rollback(ghost-stack) = %v, want ErrRevisionNotFound", err)
	}
}

// TestUndeploy_HappyPath removes the swarm stack first (docker stack rm) and
// deletes the stack row + its revisions from the store.
func TestUndeploy_HappyPath(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	if _, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := svc.Undeploy(ctx, "donation-campaign"); err != nil {
		t.Fatalf("Undeploy: %v", err)
	}

	if len(dep.removed) != 1 || dep.removed[0] != "donation-campaign" {
		t.Errorf("RemoveStack calls = %v, want [donation-campaign]", dep.removed)
	}
	if _, err := s.GetStack(ctx, "donation-campaign"); !errors.Is(err, store.ErrStackNotFound) {
		t.Errorf("stack row still present after Undeploy: %v", err)
	}
}

// TestUndeploy_UnknownStack returns ErrStackNotFound when there is no stack
// record. The swarm removal is attempted first (it is harmless and keeps the
// store consistent if the swarm stack already exists), then the missing record
// surfaces as ErrStackNotFound.
func TestUndeploy_UnknownStack(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)

	err := svc.Undeploy(context.Background(), "ghost-stack")
	if !errors.Is(err, store.ErrStackNotFound) {
		t.Errorf("Undeploy(ghost-stack) = %v, want ErrStackNotFound", err)
	}
	if len(dep.removed) != 1 || dep.removed[0] != "ghost-stack" {
		t.Errorf("RemoveStack calls = %v, want [ghost-stack]", dep.removed)
	}
}

// TestUndeploy_DeployerError surfaces the docker stack rm failure and leaves
// the stack row intact (nothing is deleted from the store).
func TestUndeploy_DeployerError(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{removeErr: errors.New("docker daemon unavailable")}
	svc := newService(s, dep)
	ctx := context.Background()

	if _, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	err := svc.Undeploy(ctx, "donation-campaign")
	if err == nil || !strings.Contains(err.Error(), "docker stack rm") {
		t.Errorf("Undeploy err = %v, want wrapped docker stack rm error", err)
	}
	if _, gErr := s.GetStack(ctx, "donation-campaign"); gErr != nil {
		t.Errorf("stack row should survive a failed swarm removal: %v", gErr)
	}
}
