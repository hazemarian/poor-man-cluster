package stacks

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
)

// nodeListerDocker answers NodeList and panics on anything else it is asked, so
// a test proves the placement check is the only swarm call the deploy makes
// before it is refused.
type nodeListerDocker struct {
	runtime.Client
	nodes []runtime.Node
	err   error
	calls int
}

func (d *nodeListerDocker) NodeList(context.Context) ([]runtime.Node, error) {
	d.calls++
	if d.err != nil {
		return nil, d.err
	}
	return d.nodes, nil
}

func placementApp(service, placement string) *dsl.App {
	return &dsl.App{
		Name:   "demo",
		Env:    "production",
		Domain: "example.com",
		Services: map[string]*dsl.Service{
			service: {Image: "nginx:1.27", Placement: placement},
		},
	}
}

var testNodes = []runtime.Node{
	{Hostname: "nxt-m-1", Role: "manager"},
	{Hostname: "nxt-w-1", Role: "worker"},
}

// TestCheckPlacementHosts_UnknownHostRefused: a misspelt pin must name the
// unknown host AND the hosts that do exist, so the operator can fix it without
// running `docker node ls` first.
func TestCheckPlacementHosts_UnknownHostRefused(t *testing.T) {
	err := checkPlacementHosts(placementApp("db", "nxt-w-2"), testNodes)
	if err == nil {
		t.Fatal("a placement naming a node the swarm does not have must be refused")
	}
	msg := err.Error()
	for _, want := range []string{`services.db.placement`, `unknown swarm node "nxt-w-2"`, "nxt-m-1", "nxt-w-1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

// TestCheckPlacementHosts_KnownHostPasses: the intended use — pinning a stateful
// service to a real node — must not be refused.
func TestCheckPlacementHosts_KnownHostPasses(t *testing.T) {
	if err := checkPlacementHosts(placementApp("db", "nxt-w-1"), testNodes); err != nil {
		t.Fatalf("a known hostname must pass, got %v", err)
	}
}

// TestCheckPlacementHosts_RoleAndEmptyPass: only hostnames are checked. A role
// names a class of node and empty means anywhere; neither can be "unknown".
func TestCheckPlacementHosts_RoleAndEmptyPass(t *testing.T) {
	for _, placement := range []string{"manager", "worker", ""} {
		if err := checkPlacementHosts(placementApp("api", placement), testNodes); err != nil {
			t.Errorf("placement %q must pass, got %v", placement, err)
		}
	}
}

// TestDeploy_UnknownPlacementHostDoesNotDeploy: the check runs in the pipeline
// before anything is recorded or applied, so a typo never reaches the swarm.
func TestDeploy_UnknownPlacementHostDoesNotDeploy(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	docker := &nodeListerDocker{nodes: testNodes}
	svc := &Service{Store: s, Deployer: dep, Docker: docker, MkdirAll: func(string, os.FileMode) error { return nil }}

	_, err := svc.Deploy(context.Background(), Payload{Manifest: `
app: pinned
env: production
domain: example.com
services:
  db:
    image: postgres:14-alpine
    placement: nxt-w-9
`})
	if err == nil {
		t.Fatal("deploy accepted a placement on a node that does not exist")
	}
	if !strings.Contains(err.Error(), `services.db.placement: unknown swarm node "nxt-w-9"`) {
		t.Errorf("error %q does not name the unknown host and the field", err)
	}
	if got := dep.callCount(); got != 0 {
		t.Errorf("deployer received %d calls, want 0 (the stack must not be deployed)", got)
	}
	if docker.calls == 0 {
		t.Error("the swarm node list was never read, so nothing was actually checked")
	}
}

// TestDeploy_KnownPlacementHostDeploys: the same pipeline with a real hostname
// must deploy exactly as before.
func TestDeploy_KnownPlacementHostDeploys(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	docker := &nodeListerDocker{nodes: testNodes}
	svc := &Service{Store: s, Deployer: dep, Docker: docker, MkdirAll: func(string, os.FileMode) error { return nil }}

	res, err := svc.Deploy(context.Background(), Payload{Manifest: `
app: pinned
env: production
domain: example.com
services:
  db:
    image: postgres:14-alpine
    placement: nxt-w-1
`})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if got := dep.callCount(); got != 1 {
		t.Fatalf("deployer received %d calls, want 1", got)
	}
	if !strings.Contains(string(res.RenderedYAML), "node.hostname == nxt-w-1") {
		t.Errorf("rendered compose does not pin the service to the node:\n%s", res.RenderedYAML)
	}
}

// TestDeploy_NodeListFailureDegrades: an unreadable node list is not evidence of
// a bad hostname. The deploy proceeds rather than failing on a transient swarm
// API error.
func TestDeploy_NodeListFailureDegrades(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	docker := &nodeListerDocker{err: errors.New("swarm is unreachable")}
	svc := &Service{Store: s, Deployer: dep, Docker: docker, MkdirAll: func(string, os.FileMode) error { return nil }}

	if _, err := svc.Deploy(context.Background(), Payload{Manifest: `
app: pinned
env: production
domain: example.com
services:
  db:
    image: postgres:14-alpine
    placement: nxt-w-1
`}); err != nil {
		t.Fatalf("an unreadable node list must not fail the deploy, got %v", err)
	}
	if got := dep.callCount(); got != 1 {
		t.Errorf("deployer received %d calls, want 1", got)
	}
}

// TestDeploy_NoHostnamePlacementSkipsNodeList: a manifest that only uses roles
// (or no placement at all) must not pay a swarm round-trip for a check that has
// nothing to check.
func TestDeploy_NoHostnamePlacementSkipsNodeList(t *testing.T) {
	s := openTestStore(t)
	dep := &recordingDeployer{}
	docker := &nodeListerDocker{nodes: testNodes}
	svc := &Service{Store: s, Deployer: dep, Docker: docker, MkdirAll: func(string, os.FileMode) error { return nil }}

	if _, err := svc.Deploy(context.Background(), Payload{Manifest: `
app: unpinned
env: production
domain: example.com
services:
  db:
    image: postgres:14-alpine
    placement: manager
`}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if docker.calls != 0 {
		t.Errorf("node list read %d times, want 0 for a role-only placement", docker.calls)
	}
}
