package cluster

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// The collector is pinned to node.labels.pmcluster.leader with a HARD constraint,
// so the label is a precondition of deploying it and a placement change has to
// move the running task. These tests drive both halves without Docker.

func TestRequireLeaderLabel(t *testing.T) {
	ctx := context.Background()

	// No orchestrator: nothing is constrained, nothing to check.
	if err := RequireLeaderLabel(ctx, nil); err != nil {
		t.Fatalf("nil client: %v", err)
	}

	// Nodes exist and none carries the label: the deploy must be refused with a
	// message that names the condition, because the alternative is a service at
	// 0/1 with no task log to explain it.
	f := newFakeDocker()
	f.nodes = []runtime.Node{
		{ID: "m1", Hostname: "nxt-sw-5-m", Role: "manager", IsLeader: true},
		{ID: "m2", Hostname: "nxt-sw-4-m", Role: "manager"},
	}
	err := RequireLeaderLabel(ctx, f)
	if err == nil {
		t.Fatal("an unlabelled swarm was accepted; the collector would never schedule")
	}
	if !strings.Contains(err.Error(), "no node carries pmcluster.leader=true") {
		t.Fatalf("error %q does not name the missing label", err)
	}
	if err.Error() != NoLeaderLabelCondition {
		t.Fatalf("error %q is not the shared condition %q", err, NoLeaderLabelCondition)
	}

	// Exactly one carrier: accepted.
	f.nodes[0].Labels = map[string]string{runtime.LeaderNodeLabel: "true"}
	if err := RequireLeaderLabel(ctx, f); err != nil {
		t.Fatalf("a labelled swarm was refused: %v", err)
	}
}

// TestClaimLeaderLabel covers the bootstrap half: `cluster up` runs BEFORE the
// daemon that maintains the label is installed, so it must claim the label for the
// current leader or the very first deploy is unschedulable.
func TestClaimLeaderLabel(t *testing.T) {
	ctx := context.Background()

	f := newFakeDocker()
	f.nodes = []runtime.Node{
		{ID: "m1", Hostname: "nxt-sw-5-m", Role: "manager", IsLeader: true},
		{ID: "m2", Hostname: "nxt-sw-4-m", Role: "manager"},
	}
	node, err := ClaimLeaderLabel(ctx, f)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if node != "nxt-sw-5-m" {
		t.Fatalf("claimed %q, want the leader nxt-sw-5-m", node)
	}
	if len(f.nodeLabels) != 1 || f.nodeLabels[0] != runtime.LeaderNodeLabel+"=true" {
		t.Fatalf("label calls = %v, want one leader=true", f.nodeLabels)
	}

	// Idempotent: a second run writes nothing.
	f.nodeLabels = nil
	if _, err := ClaimLeaderLabel(ctx, f); err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(f.nodeLabels) != 0 {
		t.Fatalf("label calls = %v, want none when already claimed", f.nodeLabels)
	}

	// A follower manager must not be labelled: only the Raft leader is.
	f2 := newFakeDocker()
	f2.nodes = []runtime.Node{{ID: "m2", Hostname: "nxt-sw-4-m", Role: "manager"}}
	if node, err := ClaimLeaderLabel(ctx, f2); err != nil || node != "" {
		t.Fatalf("claimed %q, err %v — a swarm with no leader must claim nothing", node, err)
	}
	if len(f2.nodeLabels) != 0 {
		t.Fatalf("label calls = %v, want none without a leader", f2.nodeLabels)
	}
}

// prevCollectorRender rewrites a rendered observability stack into the shape a
// fleet upgraded from this version has on disk: a manager constraint plus a leader
// PREFERENCE. Replacing that with the constraint is the change that must force the
// running task.
func prevCollectorRender(t *testing.T, rendered []byte) []byte {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		t.Fatalf("parse render: %v", err)
	}
	services, _ := doc["services"].(map[string]any)
	svc, ok := services["otel-collector"].(map[string]any)
	if !ok {
		t.Fatal("rendered stack has no otel-collector service")
	}
	deploy, _ := svc["deploy"].(map[string]any)
	placement, _ := deploy["placement"].(map[string]any)
	constraints, _ := placement["constraints"].([]any)
	kept := make([]any, 0, len(constraints))
	for _, c := range constraints {
		if s, _ := c.(string); strings.Contains(s, runtime.LeaderNodeLabel) {
			continue
		}
		kept = append(kept, c)
	}
	placement["constraints"] = kept
	placement["preferences"] = []any{
		map[string]any{"spread": "node.labels." + runtime.LeaderNodeLabel + "==true"},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal previous render: %v", err)
	}
	return out
}

func TestCollectorPlacementChanged(t *testing.T) {
	ctx := context.Background()
	fresh, err := LoadComposeFile(StackObservability, RenderInput{
		Domain:                     "example.com",
		ObservabilityAdminEmail:    "admin@example.com",
		ObservabilityAdminPassword: "not-a-real-password",
		LeaderNode:                 "nxt-sw-5-m",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	prev := prevCollectorRender(t, fresh)

	if !collectorPlacementChanged(prev, fresh) {
		t.Fatal("preference → constraint was not detected as a placement change")
	}
	if collectorPlacementChanged(fresh, fresh) {
		t.Fatal("an identical render was reported as a placement change")
	}
	if collectorPlacementChanged(nil, fresh) {
		t.Fatal("a render with no stored predecessor was reported as a placement change")
	}

	// The helper reads the collector's own placement.
	constraints, prefs := collectorPlacementOf(fresh)
	if len(prefs) != 0 {
		t.Fatalf("preferences = %v, want none on the collector", prefs)
	}
	var haveLeader bool
	for _, c := range constraints {
		if strings.Contains(c, runtime.LeaderNodeLabel) {
			haveLeader = true
		}
	}
	if !haveLeader {
		t.Fatalf("constraints = %v, want the leader label constraint", constraints)
	}

	var out bytes.Buffer
	f := newFakeDocker()
	forceCollectorIfPlacementChanged(ctx, &out, f, prev, fresh)
	if len(f.restartedServices) != 1 || f.restartedServices[0] != observabilityCollectorService {
		t.Fatalf("restarts = %v, want exactly %s", f.restartedServices, observabilityCollectorService)
	}
	if !strings.Contains(out.String(), "forced "+observabilityCollectorService) {
		t.Fatalf("output %q does not report the restart", out.String())
	}

	// Identical placement: no restart, no noise.
	f.restartedServices = nil
	out.Reset()
	forceCollectorIfPlacementChanged(ctx, &out, f, fresh, fresh)
	if len(f.restartedServices) != 0 {
		t.Fatalf("restarts = %v, want none when the placement did not move", f.restartedServices)
	}
	if out.Len() != 0 {
		t.Fatalf("output %q, want silence when nothing changed", out.String())
	}

	// A nil client is a no-op, never a panic.
	forceCollectorIfPlacementChanged(ctx, &out, nil, prev, fresh)
}

// TestUpdate_RefusesTheObservabilityDeployWithoutTheLeaderLabel: the whole point
// of failing here is that the alternative — deploying a constraint that matches no
// node — produces a service at 0/1 with nothing in the output saying why.
func TestUpdate_RefusesTheObservabilityDeployWithoutTheLeaderLabel(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()
	f := deps.Docker.(*fakeDocker)
	f.nodes = []runtime.Node{{ID: "m1", Hostname: "nxt-sw-5-m", Role: "manager", IsLeader: true}}

	deployer := &recordingDeployer{}
	deps.Deployer = deployer
	_, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0", Force: true})
	if err == nil {
		t.Fatal("Update deployed the observability stack with no node carrying the leader label")
	}
	if !strings.Contains(err.Error(), "no node carries pmcluster.leader=true") {
		t.Fatalf("error %q does not name the missing label", err)
	}
	for _, d := range deployer.deployedStacks {
		if d.Name == "observability" {
			t.Fatalf("observability was deployed anyway: %v", deployer.deployedStacks)
		}
	}
}

// TestUpdate_DeploysWhenTheLeaderLabelIsPresent is the inverse of the refusal: the
// same run succeeds once the label is there, so the guard cannot be a blanket "no".
func TestUpdate_DeploysWhenTheLeaderLabelIsPresent(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()
	f := deps.Docker.(*fakeDocker)
	f.nodes = []runtime.Node{{
		ID: "m1", Hostname: "nxt-sw-5-m", Role: "manager", IsLeader: true,
		Labels: map[string]string{runtime.LeaderNodeLabel: "true"},
	}}

	deployer := &recordingDeployer{}
	deps.Deployer = deployer
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0", Force: true}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	var obs bool
	for _, d := range deployer.deployedStacks {
		if d.Name == "observability" {
			obs = true
		}
	}
	if !obs {
		t.Fatalf("observability was not deployed with a labelled leader: %v", deployer.deployedStacks)
	}
}

// TestUpdate_ForcesTheCollectorWhenItsPlacementChanged drives the reconcile half:
// the stored render still has the leader preference, the fresh one has the
// constraint, so `docker stack deploy` alone would rewrite the spec and leave the
// task on the old node. A second, identical run must NOT force it again.
func TestUpdate_ForcesTheCollectorWhenItsPlacementChanged(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()
	f := deps.Docker.(*fakeDocker)

	row, err := deps.Store.Config(ctx, "observability-stack")
	if err != nil {
		t.Fatalf("read observability-stack: %v", err)
	}
	prev := prevCollectorRender(t, []byte(row.RenderedContent))
	if err := deps.Store.SetRendered(ctx, "observability-stack", string(prev)); err != nil {
		t.Fatalf("store previous render: %v", err)
	}

	deployer := &recordingDeployer{}
	deps.Deployer = deployer
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(f.restartedServices) != 1 || f.restartedServices[0] != observabilityCollectorService {
		t.Fatalf("restarts = %v, want exactly one %s after the placement changed",
			f.restartedServices, observabilityCollectorService)
	}

	// Second run: the stored render now matches, so nothing moves and nothing is
	// forced (a restart on every reconcile tick would be a different bug).
	f.restartedServices = nil
	deployer2 := &recordingDeployer{}
	deps.Deployer = deployer2
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("second Update: %v", err)
	}
	if len(f.restartedServices) != 0 {
		t.Fatalf("restarts = %v, want none when the placement did not change", f.restartedServices)
	}
}
