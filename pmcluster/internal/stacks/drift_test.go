package stacks

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stackdrift"
)

// driftDocker is a test-local runtime.Client that only implements ServiceList
// (the surface stackdrift.InSync + Sync read); everything else embeds the
// interface and panics if invoked.
type driftDocker struct {
	runtime.Client
	svcs []runtime.Service
	err  error
}

func (d *driftDocker) ServiceList(context.Context) ([]runtime.Service, error) {
	return d.svcs, d.err
}

// renderedHashFromCompose parses a rendered compose and returns the
// rendered-hash label value, failing the test if any service is missing the
// label or if two services disagree.
func renderedHashFromCompose(t *testing.T, yml []byte) string {
	t.Helper()
	var doc struct {
		Services map[string]struct {
			Deploy struct {
				Labels map[string]string `json:"labels"`
			} `json:"deploy"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal(yml, &doc); err != nil {
		t.Fatalf("parse rendered compose: %v", err)
	}
	if len(doc.Services) == 0 {
		t.Fatalf("rendered compose has no services:\n%s", yml)
	}
	hash := ""
	for name, srv := range doc.Services {
		h := srv.Deploy.Labels[runtime.RenderedHashLabel]
		if h == "" {
			t.Fatalf("service %q is missing the %s label:\n%s", name, runtime.RenderedHashLabel, yml)
		}
		if hash == "" {
			hash = h
		}
		if h != hash {
			t.Fatalf("service %q carries hash %q, expected %q", name, h, hash)
		}
	}
	return hash
}

// TestDeploy_StampsRenderedHashOnEveryService verifies the app-stack render
// path stamps runtime.RenderedHashLabel on EVERY service, that the value
// equals stackdrift.ContentHash of the label-free render, and that it is
// deterministic across repeated deploys of the same manifest.
func TestDeploy_StampsRenderedHashOnEveryService(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	res, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	rev, err := s.Revision(ctx, "donation-campaign", res.Revision)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	hash := renderedHashFromCompose(t, []byte(rev.RenderedYAML))

	// Re-render the same source label-free and assert the label equals its hash.
	parsed, err := manifest.Parse([]byte(donationCampaignManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := manifest.Interpolate(parsed); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if err := manifest.Validate(parsed); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	built, err := manifest.BuildIR(ctx, parsed, svc.Resolver)
	if err != nil {
		t.Fatalf("BuildIR: %v", err)
	}
	plain, err := svc.mkWriter(nil).Write(ctx, built)
	if err != nil {
		t.Fatalf("label-free render: %v", err)
	}
	if got := stackdrift.ContentHash(plain); got != hash {
		t.Errorf("stamped hash %q != stackdrift.ContentHash(label-free render) %q", hash, got)
	}

	// Deterministic: a second deploy of the same manifest stamps the same hash.
	res2, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest})
	if err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	rev2, err := s.Revision(ctx, "donation-campaign", res2.Revision)
	if err != nil {
		t.Fatalf("Revision (second): %v", err)
	}
	if hash2 := renderedHashFromCompose(t, []byte(rev2.RenderedYAML)); hash2 != hash {
		t.Errorf("hash not deterministic across deploys: %q vs %q", hash, hash2)
	}
}

// TestDeploy_OrderedLevelsStampSameHashOnEveryLevel guards the critical
// multi-level trap: every per-level (depends_on) subset compose must carry the
// SAME rendered-hash label value computed from the FULL-stack label-free
// render — never a per-subset hash, which would never match the full render
// and would force a redeploy every single pass.
func TestDeploy_OrderedLevelsStampSameHashOnEveryLevel(t *testing.T) {
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
	res, err := svc.Deploy(ctx, Payload{Manifest: manifest})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	rev, err := s.Revision(ctx, "ordered-app", res.Revision)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	fullHash := renderedHashFromCompose(t, []byte(rev.RenderedYAML))

	if len(dep.noPruneCalls) != 2 {
		t.Fatalf("DeployStackNoPrune calls = %d, want 2", len(dep.noPruneCalls))
	}
	for i, call := range dep.noPruneCalls {
		if h := renderedHashFromCompose(t, call.yaml); h != fullHash {
			t.Errorf("depends_on level %d carries hash %q, want the FULL-stack hash %q", i, h, fullHash)
		}
	}
}

// TestSync_RedeploysWhenLiveSwarmDrifted verifies Sync's live-swarm check: a
// matching hash in the store is no longer sufficient — the live services must
// also carry the fresh render's hash. A missing service or a stale label
// forces a redeploy; matching labels stay a no-op.
func TestSync_RedeploysWhenLiveSwarmDrifted(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	res, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	rev, err := s.Revision(ctx, "donation-campaign", res.Revision)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	h := renderedHashFromCompose(t, []byte(rev.RenderedYAML))

	matching := func() []runtime.Service {
		var out []runtime.Service
		for _, name := range []string{"db", "migration", "api"} {
			out = append(out, runtime.Service{
				Name:   "donation-campaign_" + name,
				Stack:  "donation-campaign",
				Labels: map[string]string{runtime.RenderedHashLabel: h},
			})
		}
		return out
	}

	// In sync: matching live labels → no-op.
	svc.Docker = &driftDocker{svcs: matching()}
	got, err := svc.Sync(ctx, "donation-campaign")
	if err != nil {
		t.Fatalf("Sync (in sync): %v", err)
	}
	if got.Changed {
		t.Error("Sync reported Changed=true when the live swarm matches the fresh render")
	}
	if len(dep.calls) != 1 {
		t.Errorf("deployer calls = %d, want 1 (no redeploy when live swarm matches)", len(dep.calls))
	}

	// Missing service → redeploy.
	svc.Docker = &driftDocker{svcs: matching()[:2]} // api is gone from the swarm
	got, err = svc.Sync(ctx, "donation-campaign")
	if err != nil {
		t.Fatalf("Sync (missing service): %v", err)
	}
	if !got.Changed {
		t.Error("Sync reported Changed=false when a live service is missing from the swarm")
	}
	if len(dep.calls) != 2 {
		t.Errorf("deployer calls = %d, want 2 (redeploy on a missing service)", len(dep.calls))
	}

	// Stale label → redeploy.
	stale := matching()
	stale[2].Labels = map[string]string{runtime.RenderedHashLabel: "stale-from-an-older-render"}
	svc.Docker = &driftDocker{svcs: stale}
	got, err = svc.Sync(ctx, "donation-campaign")
	if err != nil {
		t.Fatalf("Sync (stale label): %v", err)
	}
	if !got.Changed {
		t.Error("Sync reported Changed=false when a live service carries a stale rendered-hash label")
	}
	if len(dep.calls) != 3 {
		t.Errorf("deployer calls = %d, want 3 (redeploy on a stale label)", len(dep.calls))
	}
}

// TestSync_TransientDockerErrorTreatsAsInSync verifies a transient ServiceList
// error does NOT trigger a redeploy loop: Sync logs it and continues the no-op.
func TestSync_TransientDockerErrorTreatsAsInSync(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := newService(s, dep)
	ctx := context.Background()

	if _, err := svc.Deploy(ctx, Payload{Manifest: donationCampaignManifest}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	svc.Docker = &driftDocker{err: errors.New("docker API down")}
	got, err := svc.Sync(ctx, "donation-campaign")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got.Changed {
		t.Error("Sync reported Changed=true on a transient docker error (must treat as in sync)")
	}
	if len(dep.calls) != 1 {
		t.Errorf("deployer calls = %d, want 1 (no redeploy on a transient docker error)", len(dep.calls))
	}
}
