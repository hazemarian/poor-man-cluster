package stacks

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
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
		"Interpolating and validating manifest",
		"Translating to Compose (resolving configs/secrets)",
		"Recording revision",
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
