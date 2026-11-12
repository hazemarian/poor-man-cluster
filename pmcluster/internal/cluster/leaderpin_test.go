package cluster

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// The collector follows the leader by NAME — the same mechanism the edge uses. These
// tests drive the rendering half (the stack names the leader and must not resurrect the
// label) and the moving half (a changed name must force the running task, because
// `docker stack deploy` rewrites the spec and does not reschedule a healthy task).

func TestCollectorPlacementChanged(t *testing.T) {
	ctx := context.Background()
	render := func(leader string) []byte {
		t.Helper()
		out, err := LoadComposeFile(StackObservability, RenderInput{
			Domain:                     "example.com",
			ObservabilityAdminEmail:    "admin@example.com",
			ObservabilityAdminPassword: "not-a-real-password",
			LeaderNode:                 leader,
		})
		if err != nil {
			t.Fatalf("render for leader %q: %v", leader, err)
		}
		return out
	}
	fresh := render("nxt-sw-5-m")

	constraints, prefs := collectorPlacementOf(fresh)
	if len(prefs) != 0 {
		t.Fatalf("preferences = %v, want none on the collector", prefs)
	}
	var pinned string
	for _, c := range constraints {
		if strings.Contains(c, runtime.LeaderNodeLabel) {
			t.Fatalf("the collector is still placed by the leader LABEL (%s): that floats when leadership moves", c)
		}
		if v, ok := strings.CutPrefix(strings.ReplaceAll(c, " ", ""), "node.hostname=="); ok {
			pinned = v
		}
	}
	if pinned != "nxt-sw-5-m" {
		t.Fatalf("collector pinned to %q, want the leader nxt-sw-5-m (constraints %v)", pinned, constraints)
	}

	// A different leader is a placement change; the same render is not.
	other := render("nxt-sw-1-m")
	if !collectorPlacementChanged(other, fresh) {
		t.Fatal("a change of leader was not detected as a placement change")
	}
	if collectorPlacementChanged(fresh, fresh) {
		t.Fatal("an identical render was reported as a placement change")
	}
	if collectorPlacementChanged(nil, fresh) {
		t.Fatal("a render with no stored predecessor was reported as a placement change")
	}

	var out bytes.Buffer
	f := newFakeDocker()
	forceCollectorIfPlacementChanged(ctx, &out, f, other, fresh)
	if len(f.restartedServices) != 1 || f.restartedServices[0] != observabilityCollectorService {
		t.Fatalf("restarts = %v, want exactly %s", f.restartedServices, observabilityCollectorService)
	}
}

func TestUpdate_PlacesTheCollectorOnTheLeaderByName(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	ctx := context.Background()
	f := deps.Docker.(*fakeDocker)
	f.nodes = []runtime.Node{{ID: "m1", Hostname: "nxt-sw-5-m", Role: "manager", IsLeader: true}}

	deployer := &recordingDeployer{}
	deps.Deployer = deployer
	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0", Force: true}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	var rendered string
	for _, d := range deployer.deployedStacks {
		if d.Name == "observability" {
			rendered = d.YAML
		}
	}
	if rendered == "" {
		t.Fatalf("observability was not deployed: %v", deployer.deployedStacks)
	}
	compact := strings.ReplaceAll(rendered, " ", "")
	if strings.Contains(compact, runtime.LeaderNodeLabel+"==true") {
		t.Fatalf("the collector is still placed by the leader LABEL, which floats when leadership moves:\n%s", rendered)
	}
	if !strings.Contains(compact, "node.hostname==nxt-sw-5-m") {
		t.Fatalf("the collector is not pinned to the leader's hostname:\n%s", rendered)
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

	// Seed the store with a render that names the PREVIOUS leader — exactly what is on
	// disk after leadership moves and before anything re-renders.
	prev, err := LoadComposeFile(StackObservability, RenderInput{
		Domain:                     "example.com",
		ObservabilityAdminEmail:    "admin@example.com",
		ObservabilityAdminPassword: "not-a-real-password",
		LeaderNode:                 "nxt-sw-1-m",
	})
	if err != nil {
		t.Fatalf("render the stale stack: %v", err)
	}
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

// staleCollectorRender rewrites the collector's hostname pin to a DIFFERENT node — what a
// stored render looks like after leadership moved from that node to the current leader.
// That is the case the force exists for: the deployed spec still names the old leader.
func staleCollectorRender(t *testing.T, fresh []byte) []byte {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(fresh, &doc); err != nil {
		t.Fatalf("parse render: %v", err)
	}
	services, _ := doc["services"].(map[string]any)
	svc, ok := services["otel-collector"].(map[string]any)
	if !ok {
		t.Fatalf("no otel-collector in the render")
	}
	deploy, _ := svc["deploy"].(map[string]any)
	placement, _ := deploy["placement"].(map[string]any)
	list, _ := placement["constraints"].([]any)
	rewritten := make([]any, 0, len(list))
	for _, c := range list {
		str, _ := c.(string)
		if strings.HasPrefix(strings.ReplaceAll(str, " ", ""), "node.hostname==") {
			str = "node.hostname==nxt-sw-1-m"
		}
		rewritten = append(rewritten, str)
	}
	placement["constraints"] = rewritten
	deploy["placement"] = placement
	svc["deploy"] = deploy
	services["otel-collector"] = svc
	doc["services"] = services
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-encode render: %v", err)
	}
	return out
}
