package stacks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// recordingDeployer is a test-local implementation of cluster.StackDeployer.
// It records every (stackName, composeYAML) call and returns configurable
// errors for deploy and remove independently.
type recordingDeployer struct {
	mu           sync.Mutex
	calls        []deployCall
	noPruneCalls []deployCall
	pruned       []string
	removed      []string
	err          error
	removeErr    error
}

type deployCall struct {
	name string
	yaml []byte
}

func (r *recordingDeployer) DeployStack(_ context.Context, name string, composeYAML []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, deployCall{name: name, yaml: composeYAML})
	return r.err
}

func (r *recordingDeployer) DeployStackNoPrune(_ context.Context, name string, composeYAML []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noPruneCalls = append(r.noPruneCalls, deployCall{name: name, yaml: composeYAML})
	return r.err
}

func (r *recordingDeployer) PruneStack(_ context.Context, name string, _ []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruned = append(r.pruned, name)
	return nil
}

func (r *recordingDeployer) RemoveStack(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, name)
	return r.removeErr
}

// callCount returns how many full DeployStack calls have landed, guarding the
// shared slice for tests that read it while a background apply runs.
func (r *recordingDeployer) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// callNameAt returns the stack name of the i-th full DeployStack call.
func (r *recordingDeployer) callNameAt(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.calls) {
		return ""
	}
	return r.calls[i].name
}

func (r *recordingDeployer) ForceUpdateService(_ context.Context, _ string) error {
	return nil
}

func (r *recordingDeployer) PruneStaleContainers(_ context.Context, _ string, _ string) error {
	return nil
}

// Ensure the interface is satisfied.
var _ cluster.StackDeployer = (*recordingDeployer)(nil)

// stubDocker is a test-local implementation of runtime.Client that only
// implements the surface Undeploy uses, recording calls. Everything else
// embeds the interface (panics if invoked).
type stubDocker struct {
	runtime.Client

	secrets []string
	volumes []string

	removedSecrets []string
	removedVolumes []string
}

func (d *stubDocker) StackSecretNames(_ context.Context, _ string) ([]string, error) {
	out := make([]string, len(d.secrets))
	copy(out, d.secrets)
	return out, nil
}

func (d *stubDocker) VolumeList(_ context.Context, _, _ string) ([]string, error) {
	out := make([]string, len(d.volumes))
	copy(out, d.volumes)
	return out, nil
}

func (d *stubDocker) SecretRemove(_ context.Context, name string) error {
	d.removedSecrets = append(d.removedSecrets, name)
	return nil
}

func (d *stubDocker) VolumeRemove(_ context.Context, name string) error {
	d.removedVolumes = append(d.removedVolumes, name)
	return nil
}

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
	return &Service{Store: s, Deployer: d, MkdirAll: func(string, os.FileMode) error { return nil }}
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

// TestDeploy_RecordsPipelineSteps verifies the stored payload_json captures
// the ordered deploy step names in the envelope shape alongside the payload.
func TestDeploy_RecordsPipelineSteps(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	res, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	rev, err := s.GetRevision(ctx, "donation-campaign", res.Revision)
	if err != nil {
		t.Fatalf("GetRevision: %v", err)
	}

	var env struct {
		Payload json.RawMessage `json:"payload"`
		Steps   []string        `json:"steps"`
	}
	if err := json.Unmarshal([]byte(rev.PayloadJSON.String), &env); err != nil {
		t.Fatalf("unmarshal payload_json: %v", err)
	}
	if len(env.Steps) == 0 {
		t.Fatalf("payload_json has no steps: %s", rev.PayloadJSON.String)
	}
	want := []string{
		"Parsing manifest (DSL)",
		"Checking for app name conflicts",
		"Interpolating and validating manifest",
		"Translating to Compose (resolving configs/secrets)",
		"Recording revision",
		"Ensuring storage directories",
		"Deploying stack to the swarm",
	}
	if len(env.Steps) != len(want) {
		t.Fatalf("steps = %v, want %v", env.Steps, want)
	}
	for i := range want {
		if env.Steps[i] != want[i] {
			t.Errorf("steps[%d] = %q, want %q", i, env.Steps[i], want[i])
		}
	}

	var p Payload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("unmarshal embedded payload: %v", err)
	}
	if p.Manifest != donationCampaignManifest {
		t.Errorf("embedded payload manifest mismatch")
	}
}

// TestDecodeStoredPayload covers both the legacy (raw payload) and envelope
// (payload + steps) shapes of payload_json.
func TestDecodeStoredPayload(t *testing.T) {
	legacy := `{"app_name":"demo","manifest":"app: demo"}`
	p, steps := decodeStoredPayload(legacy)
	if p.AppName != "demo" || p.Manifest != "app: demo" {
		t.Errorf("legacy decode = %+v", p)
	}
	if steps != nil {
		t.Errorf("legacy decode steps = %v, want nil", steps)
	}

	envelope := `{"payload":{"app_name":"demo","manifest":"app: demo"},"steps":["a","b"]}`
	p, steps = decodeStoredPayload(envelope)
	if p.AppName != "demo" || p.Manifest != "app: demo" {
		t.Errorf("envelope decode payload = %+v", p)
	}
	if len(steps) != 2 || steps[0] != "a" || steps[1] != "b" {
		t.Errorf("envelope decode steps = %v, want [a b]", steps)
	}

	if p, steps := decodeStoredPayload(""); p.Manifest != "" || steps != nil {
		t.Errorf("empty decode = %+v, %v; want zero payload and nil steps", p, steps)
	}
}

// TestRollback_PreservesOriginalPayload verifies that rolling back from a
// revision recorded with the envelope shape still reconstructs "original" as
// the legacy payload (backward-compatible reconstruction).
func TestRollback_PreservesOriginalPayload(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	r1, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest, Version: "v1"})
	if err != nil {
		t.Fatalf("Deploy v1: %v", err)
	}
	time.Sleep(2 * time.Second)

	rr, err := svc.Rollback(ctx, "donation-campaign", r1.Revision)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	rev, err := s.GetRevision(ctx, "donation-campaign", rr.Revision)
	if err != nil {
		t.Fatalf("GetRevision: %v", err)
	}
	var rb struct {
		RollbackOf int64           `json:"rollback_of"`
		Original   json.RawMessage `json:"original"`
	}
	if err := json.Unmarshal([]byte(rev.PayloadJSON.String), &rb); err != nil {
		t.Fatalf("unmarshal rollback payload: %v", err)
	}
	var original Payload
	if err := json.Unmarshal(rb.Original, &original); err != nil {
		t.Fatalf("rollback 'original' is not a legacy payload: %v", err)
	}
	if original.Manifest != donationCampaignManifest || original.Version != "v1" {
		t.Errorf("rollback original = %+v, want v1 payload", original)
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

// TestDeploy_ConflictingRepoRejected verifies the app-name duplication guard:
// a second deploy of the same app name from a DIFFERENT repo must fail before
// any stack/deployer side effects.
func TestDeploy_ConflictingRepoRejected(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	if _, err := svc.Deploy(ctx, Payload{
		Manifest: donationCampaignManifest,
		RepoURL:  "https://github.com/nextrum-sy/donation-campaign",
		File:     "deploy/stg/donation-campaign.yaml",
	}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	_, err := svc.Deploy(ctx, Payload{
		Manifest: donationCampaignManifest,
		RepoURL:  "https://github.com/other-org/donation-campaign",
		File:     "deploy/stg/donation-campaign.yaml",
	})
	if err == nil {
		t.Fatal("second Deploy from a different repo: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists from repo") {
		t.Errorf("error = %q, want 'already exists from repo'", err)
	}

	// The conflicting deploy must not have recorded a new revision.
	revs, err := s.ListRevisions(ctx, "donation-campaign", 10)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 1 {
		t.Errorf("revision count = %d, want 1 (conflicting deploy recorded nothing)", len(revs))
	}
	if len(dep.calls) != 1 {
		t.Errorf("deployer calls = %d, want 1 (conflicting deploy never reached the swarm)", len(dep.calls))
	}
}

// TestDeploy_SameRepoRedeployAllowed verifies that redeploying an existing app
// name from the SAME repo (the normal webhook flow) is not blocked.
func TestDeploy_SameRepoRedeployAllowed(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	repo := "https://github.com/nextrum-sy/donation-campaign"
	if _, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest, RepoURL: repo, File: "deploy/stg/donation-campaign.yaml"}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	res, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest, RepoURL: repo, File: "deploy/stg/donation-campaign.yaml"})
	if err != nil {
		t.Fatalf("second Deploy from the same repo: %v", err)
	}
	if res.Revision == 0 {
		t.Error("second Deploy revision is 0, want a new revision")
	}
}

// TestDeploy_NoProvenanceAllowed verifies that a deploy with no repo_url (CLI
// without --repo, or legacy pre-provenance stacks) is not blocked by the guard.
func TestDeploy_NoProvenanceAllowed(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	if _, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest, RepoURL: "https://github.com/nextrum-sy/donation-campaign"}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	// No RepoURL on the second deploy — must not be blocked.
	res, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest})
	if err != nil {
		t.Fatalf("Deploy without provenance: %v", err)
	}
	if res.Revision == 0 {
		t.Error("Deploy revision is 0, want a new revision")
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
// matches v1's (rollback re-translates the stored SOURCE manifest, never the
// stored rendered YAML — the version override v2 applied is not part of the
// source), and the stack's current_revision points to the new revision.
func TestRollback_HappyPath(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	r1, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest})
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

// TestUndeploy_HappyPath removes the swarm stack first (docker stack rm),
// then the stack's named volumes + mounted secrets (docker cleanup), then
// deletes the stack row + revisions + service-scope configs/secrets.
func TestUndeploy_HappyPath(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	dk := &stubDocker{secrets: []string{"donation_campaign_db_password"}, volumes: []string{"db_data"}}
	svc := newService(s, dep)
	svc.Docker = dk
	ctx := context.Background()

	if _, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if _, err := s.CreateConfig(ctx, "service", "donation-campaign", "donation-campaign_env", "env", "A=1", "v1"); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	if err := svc.Undeploy(ctx, "donation-campaign"); err != nil {
		t.Fatalf("Undeploy: %v", err)
	}

	if len(dep.removed) != 1 || dep.removed[0] != "donation-campaign" {
		t.Errorf("RemoveStack calls = %v, want [donation-campaign]", dep.removed)
	}
	if got := dk.removedVolumes; len(got) != 1 || got[0] != "db_data" {
		t.Errorf("removedVolumes = %v, want [db_data]", got)
	}
	if got := dk.removedSecrets; len(got) != 1 || got[0] != "donation_campaign_db_password" {
		t.Errorf("removedSecrets = %v, want [donation_campaign_db_password]", got)
	}
	if _, err := s.GetStack(ctx, "donation-campaign"); !errors.Is(err, store.ErrStackNotFound) {
		t.Errorf("stack row still present after Undeploy: %v", err)
	}
	if _, err := s.GetConfig(ctx, "donation-campaign_env"); !errors.Is(err, store.ErrConfigNotFound) {
		t.Errorf("stack config still present after Undeploy: %v", err)
	}
}

// TestUndeploy_NoDocker skips the swarm-asset cleanup when no Docker client is
// wired (CLI deploy command path) but still removes the stack record.
func TestUndeploy_NoDocker(t *testing.T) {
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
	if _, err := s.GetStack(ctx, "donation-campaign"); !errors.Is(err, store.ErrStackNotFound) {
		t.Errorf("stack row still present after Undeploy: %v", err)
	}
}

// TestUndeploy_UnknownStack returns ErrStackNotFound when there is no stack
// record. The existence check happens first, so no swarm call is made.
func TestUndeploy_UnknownStack(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	dk := &stubDocker{secrets: []string{"s"}, volumes: []string{"v"}}
	svc := newService(s, dep)
	svc.Docker = dk

	err := svc.Undeploy(context.Background(), "ghost-stack")
	if !errors.Is(err, store.ErrStackNotFound) {
		t.Errorf("Undeploy(ghost-stack) = %v, want ErrStackNotFound", err)
	}
	if len(dep.removed) != 0 {
		t.Errorf("RemoveStack calls = %v, want [] (no stack record)", dep.removed)
	}
	if len(dk.removedVolumes) != 0 || len(dk.removedSecrets) != 0 {
		t.Errorf("cleanup ran for unknown stack: vols=%v secs=%v", dk.removedVolumes, dk.removedSecrets)
	}
}

// TestSync_NoOpWhenNothingChanged verifies that syncing a stack whose source
// manifest re-translates to the same rendered compose is a no-op: no new
// revision, no Docker call, Changed=false.
func TestSync_NoOpWhenNothingChanged(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	res, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	revBefore := res.Revision

	got, err := svc.Sync(ctx, "donation-campaign")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got.Changed {
		t.Error("Sync reported Changed=true on identical rendered content")
	}
	if got.Revision != revBefore {
		t.Errorf("Sync revision = %d, want %d (no new revision)", got.Revision, revBefore)
	}
	if len(dep.calls) != 1 {
		t.Errorf("deployer received %d calls, want 1 (sync must not re-deploy)", len(dep.calls))
	}
	st, err := s.GetStack(ctx, "donation-campaign")
	if err != nil {
		t.Fatalf("GetStack: %v", err)
	}
	if st.CurrentRevision != revBefore {
		t.Errorf("current_revision = %d, want %d (no new revision recorded)", st.CurrentRevision, revBefore)
	}
}

// TestSync_NoOpWithoutStoredHash verifies the safety guard: a revision recorded
// before the rendered_hash feature (empty hash) must NOT be treated as a
// no-op — sync re-deploys to establish a hash baseline.
func TestSync_NoOpWithoutStoredHash(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	if err := s.RecordDeploy(ctx, &store.StackRevision{
		StackName:    "legacy",
		Revision:     1000,
		SourceYAML:   "app: legacy\nenv: production\ndomain: example.test\nservices:\n  web:\n    image: nginx\n",
		RenderedYAML: "services:\n  web:\n    image: nginx\n",
	}, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}

	got, err := svc.Sync(ctx, "legacy")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !got.Changed {
		t.Error("Sync with empty stored hash should re-deploy, not report no-change")
	}
	if len(dep.calls) != 1 {
		t.Errorf("deployer received %d calls, want 1", len(dep.calls))
	}
}

// TestSync_RedeploysWhenConfigChanged verifies that a config() edit in the DB
// changes the rendered compose, so sync re-deploys and records a new revision.
func TestSync_RedeploysWhenConfigChanged(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	const manifest = `
app: cfg-app
env: production
domain: example.test
services:
  web:
    image: nginx
    env:
      ADMIN_FLAG: config(admin_flag)
`
	// Seed the config the manifest references.
	if _, err := s.CreateConfig(ctx, "service", "", "admin_flag", "env", "false", "v0.2.50"); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	svc.Resolver = &StoreConfigResolver{Store: s}

	res, err := svc.Deploy(ctx, Payload{Manifest: manifest})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	revBefore := res.Revision
	if len(dep.calls) != 1 {
		t.Fatalf("deployer received %d calls, want 1", len(dep.calls))
	}

	// Operator edits the config in the DB — rendered output must now differ.
	if _, err := s.UpdateConfig(ctx, "admin_flag", "true", "v0.2.50"); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	got, err := svc.Sync(ctx, "cfg-app")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !got.Changed {
		t.Error("Sync reported Changed=false despite config edit changing rendered output")
	}
	if got.Revision == revBefore {
		t.Error("Sync should have recorded a new revision after a config change")
	}
	if len(dep.calls) != 2 {
		t.Errorf("deployer received %d calls, want 2 (redeploy after config change)", len(dep.calls))
	}
	if !strings.Contains(string(dep.calls[1].yaml), "ADMIN_FLAG: \"true\"") {
		t.Errorf("redeployed compose does not contain the updated config value: %s", dep.calls[1].yaml)
	}
}

// TestSync_UnknownStackErrors verifies sync on a stack with no revisions
// returns a clear error.
func TestSync_UnknownStackErrors(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)

	_, err := svc.Sync(context.Background(), "ghost")
	if err == nil {
		t.Fatal("Sync on unknown stack should error")
	}
	if !strings.Contains(err.Error(), "no revisions") {
		t.Errorf("error should mention missing revisions: %v", err)
	}
	if len(dep.calls) != 0 {
		t.Errorf("deployer received %d calls, want 0", len(dep.calls))
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

// TestDeploy_OrderedLevels deploys a stack whose services form depends_on
// levels (db ← migration, api) and verifies the deployer receives each level
// as its own compose via DeployStackNoPrune, in topological order, followed by
// exactly ONE PruneStack call with the full stack compose. The single-level
// DeployStack path must NOT be used.
func TestDeploy_OrderedLevels(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	const manifest = `
app: ordered-app
env: production
domain: example.com
services:
  db:
    image: postgres:14-alpine
    volumes: [db_data:/var/lib/postgresql/data]
  migration:
    image: ghcr.io/acme/app:latest
    command: [./migrate]
    run_once: true
    depends_on: [db]
  api:
    image: ghcr.io/acme/app:latest
    replicas: 1
    depends_on: [db]
    expose:
      port: 8080
      host: api.ordered-app.example.com
`

	result, err := svc.Deploy(ctx, Payload{Manifest: manifest})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if result.Revision == 0 {
		t.Error("result.Revision is 0")
	}

	// Level 0 (db) + level 1 (migration, api) via the no-prune path.
	if len(dep.noPruneCalls) != 2 {
		t.Fatalf("DeployStackNoPrune calls = %d, want 2 (db then migration+api)", len(dep.noPruneCalls))
	}
	// Full-stack DeployStack must NOT run for an ordered deploy.
	if len(dep.calls) != 0 {
		t.Errorf("DeployStack (full) calls = %d, want 0 for an ordered deploy", len(dep.calls))
	}
	for i, call := range dep.noPruneCalls {
		if call.name != "ordered-app" {
			t.Errorf("noPrune call %d name = %q, want ordered-app", i, call.name)
		}
		if len(call.yaml) == 0 {
			t.Errorf("noPrune call %d received empty compose", i)
		}
	}
	// Level 0 must declare db; level 1 must declare migration + api.
	if !strings.Contains(string(dep.noPruneCalls[0].yaml), "db:") {
		t.Errorf("level 0 compose should declare db:\n%s", dep.noPruneCalls[0].yaml)
	}
	level1 := string(dep.noPruneCalls[1].yaml)
	if !strings.Contains(level1, "migration:") || !strings.Contains(level1, "api:") {
		t.Errorf("level 1 compose should declare migration + api:\n%s", level1)
	}
	if strings.Contains(string(dep.noPruneCalls[0].yaml), "migration:") {
		t.Errorf("level 0 compose must not declare migration:\n%s", dep.noPruneCalls[0].yaml)
	}

	// Exactly one prune pass, with the full stack compose.
	if len(dep.pruned) != 1 || dep.pruned[0] != "ordered-app" {
		t.Errorf("PruneStack calls = %v, want exactly [ordered-app]", dep.pruned)
	}
}

// TestDeploy_NoDepsUsesFullDeploy: a stack with no depends_on keeps the
// today's single full DeployStack path (no per-level calls, no separate
// prune).
func TestDeploy_NoDepsUsesFullDeploy(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	if _, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(dep.calls) != 1 {
		t.Errorf("DeployStack calls = %d, want 1", len(dep.calls))
	}
	if len(dep.noPruneCalls) != 0 || len(dep.pruned) != 0 {
		t.Errorf("no-deps stack must not use the ordered path (noPrune=%d prune=%d)", len(dep.noPruneCalls), len(dep.pruned))
	}
}

// TestDeployAsync_ReturnsEarlyAndAppliesInBackground: DeployAsync validates,
// records the revision, and returns immediately while the swarm apply runs in
// a background goroutine (detached from the caller context). The result is
// available with the new revision; the apply lands on the deployer shortly
// after.
func TestDeployAsync_ReturnsEarlyAndAppliesInBackground(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	start := time.Now()
	res, err := svc.DeployAsync(ctx, Payload{Manifest: donationCampaignManifest})
	if err != nil {
		t.Fatalf("DeployAsync: %v", err)
	}
	if res == nil || res.StackName != "donation-campaign" || res.Revision == 0 {
		t.Fatalf("DeployAsync result = %+v, want stack donation-campaign + revision", res)
	}
	// The apply is backgrounded — give it a bounded moment to reach the
	// deployer, then confirm it did apply.
	deadline := time.Now().Add(2 * time.Second)
	for dep.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if dep.callCount() != 1 {
		t.Errorf("DeployStack calls = %d, want 1 (background apply)", dep.callCount())
	}
	if dep.callNameAt(0) != "donation-campaign" {
		t.Errorf("applied stack = %q, want donation-campaign", dep.callNameAt(0))
	}
	if time.Since(start) > time.Second {
		t.Errorf("DeployAsync returned in %s — want immediate return while the apply is backgrounded", time.Since(start))
	}
}

// TestDeployAsync_ValidationIsSynchronous: manifest validation errors surface
// immediately from DeployAsync — the background apply never starts.
func TestDeployAsync_ValidationIsSynchronous(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	_, err := svc.DeployAsync(ctx, Payload{Manifest: "app: broken\nservices:\n  web:\n    image: nginx\n    expose:\n      port: not-a-number"})
	if err == nil {
		t.Fatal("DeployAsync accepted an invalid manifest")
	}
	time.Sleep(50 * time.Millisecond)
	if len(dep.calls) != 0 {
		t.Errorf("deployer called %d times on a validation error, want 0", len(dep.calls))
	}
}

// TestDeployAsync_BackgroundFailureRecordsLastError: when the background
// apply fails, the stack's last_error is persisted so the console can surface
// it even though the fire-and-forget response already returned 202. The
// outcome lands in the stack's JSON error history (newest first): the failed
// apply appends an entry with the error; a later successful deploy appends a
// success entry (empty Error) that masks older failures for the display while
// the history is retained.
func TestDeployAsync_BackgroundFailureRecordsLastError(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{err: errors.New("docker stack deploy: boom")}
	svc := newService(s, dep)
	ctx := context.Background()

	if _, err := svc.DeployAsync(ctx, Payload{Manifest: donationCampaignManifest}); err != nil {
		t.Fatalf("DeployAsync: %v", err)
	}

	// The apply is backgrounded; wait for it to fail and persist.
	deadline := time.Now().Add(2 * time.Second)
	var st *store.Stack
	var err error
	for time.Now().Before(deadline) {
		st, err = s.GetStack(ctx, "donation-campaign")
		entries := store.ParseStackErrors(st.LastError)
		if err == nil && len(entries) > 0 && entries[0].Error != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GetStack: %v", err)
	}
	entries := store.ParseStackErrors(st.LastError)
	if len(entries) == 0 || entries[0].Error != "docker stack deploy: docker stack deploy: boom" {
		t.Errorf("stack error history = %q, want the background apply error newest", st.LastError)
	}

	// A subsequent successful deploy writes NOTHING (only failures are
	// recorded) — the display clears via the revision join (current revision
	// has no failure entry), and the history still retains the prior failure.
	dep.err = nil
	if _, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest}); err != nil {
		t.Fatalf("Deploy after recovery: %v", err)
	}
	st, _ = s.GetStack(ctx, "donation-campaign")
	entries = store.ParseStackErrors(st.LastError)
	if len(entries) == 0 || entries[0].Error == "" {
		t.Errorf("stack error history after successful deploy = %q, want failures-only (prior failure still newest)", st.LastError)
	}
	// The prior failure must still be retained in the history.
	seen := false
	for _, e := range entries {
		if e.Error == "docker stack deploy: docker stack deploy: boom" {
			seen = true
			break
		}
	}
	if !seen {
		t.Errorf("stack error history after successful deploy = %q, want the prior failure retained", st.LastError)
	}
}

// TestDeploy_StructuredDebugLogs: the ordered-deploy report is emitted as
// structured zerolog records — the pipeline steps and service names at INFO
// level (never the YAML bodies), plus the per-level compose YAML at DEBUG
// level so info stays calm and debug carries everything.
func TestDeploy_StructuredDebugLogs(t *testing.T) {
	const manifest = `
app: debug-app
env: production
domain: example.com
services:
  db:
    image: postgres:14-alpine
    volumes: [db_data:/var/lib/postgresql/data]
  migration:
    image: ghcr.io/acme/app:latest
    command: [./migrate]
    run_once: true
    depends_on: [db]
`

	deploy := func(log zerolog.Logger) (string, error) {
		s := openTestStore(t)
		dep := &recordingDeployer{}
		svc := newService(s, dep)
		svc.Log = log
		_, err := svc.Deploy(context.Background(), Payload{Manifest: manifest})
		return "", err
	}

	var dbgBuf, infoBuf bytes.Buffer
	dbgLog := zerolog.New(&dbgBuf).Level(zerolog.DebugLevel)
	infoLog := zerolog.New(&infoBuf).Level(zerolog.InfoLevel)

	if _, err := deploy(dbgLog); err != nil {
		t.Fatalf("deploy (debug logger): %v", err)
	}
	if _, err := deploy(infoLog); err != nil {
		t.Fatalf("deploy (info logger): %v", err)
	}

	out := dbgBuf.String()
	for _, want := range []string{
		"per-service docker stack deploy",
		"waiting for level",
		"one drift-prune pass",
		"compose_yaml",       // the per-level + full compose YAML is logged
		"postgres:14-alpine", // ... and its body is present
		"depends_on",         // the subset compose keeps the depends_on list
	} {
		if !strings.Contains(out, want) {
			t.Errorf("debug log missing %q", want)
		}
	}
	if infoBuf.Len() == 0 {
		t.Errorf("info-level logger emitted 0 bytes — the deploy pipeline must log its steps at info level")
	}
	inf := infoBuf.String()
	for _, want := range []string{
		"deploy — starting pipeline",
		"deploy — manifest parsed",
		"deploy stack — deploying depends_on level",
		"deploy stack — waiting for level to become healthy",
		"deploy stack — level healthy",
		"deploy — completed",
		`"stack":"debug-app"`, // app name attached to the completed record
		"db",                  // service names in the info records
		"migration",
	} {
		if !strings.Contains(inf, want) {
			t.Errorf("info log missing %q", want)
		}
	}
	for _, notWant := range []string{
		"compose_yaml",
		"postgres:14-alpine",
		"driver_opts",
	} {
		if strings.Contains(inf, notWant) {
			t.Errorf("info log must not contain %q (YAML stays debug-only)", notWant)
		}
	}
}

// TestDeploy_CycleErrors: a depends_on cycle is rejected before any deploy.
func TestDeploy_CycleErrors(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	const manifest = `
app: cyc-app
env: production
domain: example.com
services:
  a:
    image: nginx:latest
    depends_on: [b]
  b:
    image: nginx:latest
    depends_on: [a]
`
	_, err := svc.Deploy(ctx, Payload{Manifest: manifest})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("Deploy(cycle) = %v, want a cycle error", err)
	}
	if len(dep.calls) != 0 || len(dep.noPruneCalls) != 0 {
		t.Errorf("cycle deploy must not reach the deployer (calls=%d noPrune=%d)", len(dep.calls), len(dep.noPruneCalls))
	}
}

// TestDeploy_UnknownDepErrors: depends_on referencing a missing service is
// rejected before any deploy.
func TestDeploy_UnknownDepErrors(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	const manifest = `
app: dep-app
env: production
domain: example.com
services:
  db:
    image: postgres:14-alpine
  migration:
    image: ghcr.io/acme/app:latest
    command: [./migrate]
    run_once: true
    depends_on: [ghost]
`
	_, err := svc.Deploy(ctx, Payload{Manifest: manifest})
	if err == nil || !strings.Contains(err.Error(), "no such service") {
		t.Fatalf("Deploy(unknown dep) = %v, want a no-such-service error", err)
	}
	if len(dep.calls) != 0 || len(dep.noPruneCalls) != 0 {
		t.Errorf("unknown-dep deploy must not reach the deployer (calls=%d noPrune=%d)", len(dep.calls), len(dep.noPruneCalls))
	}
}

// waitFake is a test-local runtime.Client that only implements the surface
// waitLevelHealthy uses (ServiceList + ServiceTasks); everything else embeds
// the interface and panics if invoked.
type waitFake struct {
	runtime.Client
	svcs  []runtime.Service
	tasks map[string][]runtime.ServiceTask
}

func (f *waitFake) ServiceList(context.Context) ([]runtime.Service, error) { return f.svcs, nil }

func (f *waitFake) ServiceTasks(_ context.Context, id string) ([]runtime.ServiceTask, error) {
	return f.tasks[id], nil
}

func task(id, state string, started int64) runtime.ServiceTask {
	return runtime.ServiceTask{TaskID: id, State: state, StartedAt: started}
}

// waitService wires a Service with a Docker client so waitLevelHealthy
// actually polls, using tight timeouts for fast tests.
func waitService(t *testing.T, f *waitFake) *Service {
	t.Helper()
	oldTimeout, oldInterval := deployWaitTimeout, deployWaitInterval
	deployWaitTimeout, deployWaitInterval = 400*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() {
		deployWaitTimeout, deployWaitInterval = oldTimeout, oldInterval
	})
	return &Service{Docker: f, MkdirAll: func(string, os.FileMode) error { return nil }}
}

// TestWaitLevelHealthy_RunOnceRetrySucceeded: a run-once job that failed once
// then succeeded on retry leaves [failed, complete] task records. The deploy
// must treat it as READY (the newest terminal task wins), not abort.
func TestWaitLevelHealthy_RunOnceRetrySucceeded(t *testing.T) {
	f := &waitFake{
		svcs: []runtime.Service{
			{Name: "demo_migration", RunOnce: true, Desired: 1, Replicas: 0},
		},
		tasks: map[string][]runtime.ServiceTask{
			"demo_migration": {task("t1", "failed", 100), task("t2", "complete", 200)},
		},
	}
	svc := waitService(t, f)
	if err := svc.waitLevelHealthy(context.Background(), "demo", []string{"migration"}); err != nil {
		t.Fatalf("waitLevelHealthy([failed, complete]) = %v, want ready (nil)", err)
	}
}

// TestWaitLevelHealthy_RunOnceAllFailed: every attempt failed -> the deploy
// stops with the task error.
func TestWaitLevelHealthy_RunOnceAllFailed(t *testing.T) {
	f := &waitFake{
		svcs: []runtime.Service{
			{Name: "demo_migration", RunOnce: true, Desired: 1, Replicas: 0},
		},
		tasks: map[string][]runtime.ServiceTask{
			"demo_migration": {task("t1", "failed", 100), task("t2", "rejected", 200)},
		},
	}
	svc := waitService(t, f)
	err := svc.waitLevelHealthy(context.Background(), "demo", []string{"migration"})
	if err == nil || !strings.Contains(err.Error(), "failed during ordered deploy") {
		t.Fatalf("waitLevelHealthy(all failed) = %v, want a run-once failure error", err)
	}
}

// TestWaitLevelHealthy_RunOnceActiveAttempt: an attempt is in flight
// (running), even alongside an older complete record from a previous deploy —
// keep waiting (timeout), never abort and never report ready early.
func TestWaitLevelHealthy_RunOnceActiveAttempt(t *testing.T) {
	f := &waitFake{
		svcs: []runtime.Service{
			{Name: "demo_migration", RunOnce: true, Desired: 1, Replicas: 0},
		},
		tasks: map[string][]runtime.ServiceTask{
			"demo_migration": {task("t1", "complete", 100), task("t2", "running", 200)},
		},
	}
	svc := waitService(t, f)
	err := svc.waitLevelHealthy(context.Background(), "demo", []string{"migration"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("waitLevelHealthy(running attempt) = %v, want a timeout (still waiting)", err)
	}
}

// TestWaitLevelHealthy_RunOnceShutdownWithoutComplete: the job was stopped
// mid-flight and never completed — an error, not readiness.
func TestWaitLevelHealthy_RunOnceShutdownWithoutComplete(t *testing.T) {
	f := &waitFake{
		svcs: []runtime.Service{
			{Name: "demo_migration", RunOnce: true, Desired: 1, Replicas: 0},
		},
		tasks: map[string][]runtime.ServiceTask{
			"demo_migration": {task("t1", "shutdown", 100)},
		},
	}
	svc := waitService(t, f)
	err := svc.waitLevelHealthy(context.Background(), "demo", []string{"migration"})
	if err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("waitLevelHealthy(shutdown only) = %v, want a did-not-complete error", err)
	}
}

// TestWaitLevelHealthy_MixedLevel: a level containing a long-running service
// (db, converged 1/1) AND a run-once job (migration, retried then complete)
// is ready only when BOTH conditions hold.
func TestWaitLevelHealthy_MixedLevel(t *testing.T) {
	f := &waitFake{
		svcs: []runtime.Service{
			{Name: "demo_db", Desired: 1, Replicas: 1},
			{Name: "demo_migration", RunOnce: true, Desired: 1, Replicas: 0},
		},
		tasks: map[string][]runtime.ServiceTask{
			"demo_migration": {task("t1", "failed", 100), task("t2", "complete", 200)},
		},
	}
	svc := waitService(t, f)
	if err := svc.waitLevelHealthy(context.Background(), "demo", []string{"db", "migration"}); err != nil {
		t.Fatalf("waitLevelHealthy(db 1/1 + migration complete) = %v, want ready", err)
	}
}

// TestWaitLevelHealthy_MixedLevelDBPending: same level but db has not
// converged (0/1) — keep waiting until the timeout, even though the migration
// is already complete.
func TestWaitLevelHealthy_MixedLevelDBPending(t *testing.T) {
	f := &waitFake{
		svcs: []runtime.Service{
			{Name: "demo_db", Desired: 1, Replicas: 0},
			{Name: "demo_migration", RunOnce: true, Desired: 1, Replicas: 0},
		},
		tasks: map[string][]runtime.ServiceTask{
			"demo_migration": {task("t1", "complete", 100)},
		},
	}
	svc := waitService(t, f)
	err := svc.waitLevelHealthy(context.Background(), "demo", []string{"db", "migration"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("waitLevelHealthy(db 0/1) = %v, want a timeout (db not ready)", err)
	}
}
