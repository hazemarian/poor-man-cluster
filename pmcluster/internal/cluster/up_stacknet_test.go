package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// orderedClient records the sequence of NetworkCreate calls alongside the
// deploy calls (see orderedDeployer) so a test can assert the ORDER in which
// pmcluster touches each resource. It also lets a test force NetworkRemove to
// fail, to model a network that is still in use.
type orderedClient struct {
	*fakeDocker
	log          *[]string
	netRemoveErr error
}

func (c *orderedClient) NetworkCreate(ctx context.Context, spec runtime.NetworkSpec) error {
	*c.log = append(*c.log, "network-create:"+spec.Name)
	return c.fakeDocker.NetworkCreate(ctx, spec)
}

func (c *orderedClient) NetworkRemove(ctx context.Context, name string) error {
	if c.netRemoveErr != nil {
		return c.netRemoveErr
	}
	return c.fakeDocker.NetworkRemove(ctx, name)
}

// orderedDeployer records DeployStack calls into the shared ordering log.
type orderedDeployer struct {
	*recordingDeployer
	log *[]string
}

func (d *orderedDeployer) DeployStack(ctx context.Context, name string, composeYAML []byte) error {
	*d.log = append(*d.log, "deploy:"+name)
	return d.recordingDeployer.DeployStack(ctx, name, composeYAML)
}

// Compile-time assertions (embedding must still satisfy the interfaces).
var (
	_ runtime.Client = (*orderedClient)(nil)
	_ StackDeployer  = (*orderedDeployer)(nil)
)

func indexOf(log []string, want string) int {
	for i, e := range log {
		if e == want {
			return i
		}
	}
	return -1
}

// TestUp_EnsuresStackNetworkBeforeDeploy is the regression guard for the
// intermittent swarm-e2e failure
//
//	failed to create service backup_control-plane-backup:
//	  Error response from daemon: network backup_net not found
//
// The backup stack (unlike infra/edge/observability/sso) joins a PRIVATE
// overlay, backup_net. `docker stack deploy` attaches its services to that
// network BY NAME and only creates it when a label-scoped lookup does not
// already list it, so the network must exist — created by pmcluster, with the
// com.docker.stack.namespace label docker looks it up by — BEFORE the backup
// stack is deployed. If this ordering regresses (the network is left for the
// deploy to create in the same call as the service that joins it, or the label
// is dropped so docker tries to create it a second time), the e2e goes
// intermittent again.
func TestUp_EnsuresStackNetworkBeforeDeploy(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, f, deployer := newUpDeps(t)
	var log []string
	deps.Docker = &orderedClient{fakeDocker: f, log: &log}
	deps.Deployer = &orderedDeployer{recordingDeployer: deployer, log: &log}

	if _, err := Up(context.Background(), deps, UpInput{
		Domain:                  "test.example.com",
		CertPath:                certPath,
		KeyPath:                 keyPath,
		ObservabilityAdminEmail: "ops@example.com",
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// The network must have been created, and created BEFORE the deploy.
	createAt := indexOf(log, "network-create:backup_net")
	deployAt := indexOf(log, "deploy:"+string(StackBackup))
	if createAt < 0 {
		t.Fatalf("backup_net was never created by pmcluster; ordering log:\n%v", log)
	}
	if deployAt < 0 {
		t.Fatalf("backup stack was never deployed; ordering log:\n%v", log)
	}
	if createAt > deployAt {
		t.Errorf("backup_net created AFTER the backup stack was deployed (create@%d deploy@%d) — "+
			"the network must exist before the service that joins it:\n%v", createAt, deployAt, log)
	}

	// The label is the whole reason the deploy REUSES the network instead of
	// trying to create a second one with the same name.
	spec, ok := f.networks["backup_net"]
	if !ok {
		t.Fatalf("backup_net missing from the fake daemon after Up")
	}
	if got := spec.Labels[runtime.StackNamespaceLabel]; got != string(StackBackup) {
		t.Errorf("backup_net %s label = %q, want %q (docker stack deploy looks the network up by it)",
			runtime.StackNamespaceLabel, got, StackBackup)
	}

	// The stacks that join shared networks must NOT get a private overlay.
	for _, stray := range []string{"network-create:infra_net", "network-create:edge_net", "network-create:observability_net"} {
		if indexOf(log, stray) >= 0 {
			t.Errorf("unexpected private overlay created: %s\nlog:\n%v", stray, log)
		}
	}
}

// TestUp_ReplacesStaleStackNetwork asserts a leftover backup_net (e.g. from a
// previous `cluster down` whose asynchronous removal is still in flight) is
// removed and recreated before the deploy, so the deploy never lists a stale
// name it then cannot resolve.
func TestUp_ReplacesStaleStackNetwork(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, f, deployer := newUpDeps(t)
	// A stale leftover: present but no longer valid.
	f.networks["backup_net"] = runtime.NetworkSpec{Name: "backup_net"}

	var log []string
	deps.Docker = &orderedClient{fakeDocker: f, log: &log}
	deps.Deployer = &orderedDeployer{recordingDeployer: deployer, log: &log}

	if _, err := Up(context.Background(), deps, UpInput{
		Domain:                  "test.example.com",
		CertPath:                certPath,
		KeyPath:                 keyPath,
		ObservabilityAdminEmail: "ops@example.com",
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if !contains(f.removedNetworks, "backup_net") {
		t.Errorf("stale backup_net was not removed; removed=%v", f.removedNetworks)
	}
	spec, ok := f.networks["backup_net"]
	if !ok {
		t.Fatalf("backup_net was not recreated after the stale one was removed")
	}
	if spec.Labels[runtime.StackNamespaceLabel] != string(StackBackup) {
		t.Errorf("recreated backup_net is missing the %s label", runtime.StackNamespaceLabel)
	}
	createAt := indexOf(log, "network-create:backup_net")
	deployAt := indexOf(log, "deploy:"+string(StackBackup))
	if createAt < 0 || deployAt < 0 || createAt > deployAt {
		t.Errorf("recreated backup_net must precede the deploy (create@%d deploy@%d):\n%v", createAt, deployAt, log)
	}
}

// TestUp_KeepsInUseStackNetwork asserts that when a stack network cannot be
// removed (still in use by running services) the ensure leaves it alone — the
// correct, stable state the deploy will simply reuse — rather than failing the
// bring-up or clobbering it.
func TestUp_KeepsInUseStackNetwork(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, f, deployer := newUpDeps(t)
	inUse := runtime.NetworkSpec{Name: "backup_net", Labels: map[string]string{runtime.StackNamespaceLabel: string(StackBackup)}}
	f.networks["backup_net"] = inUse

	var log []string
	deps.Docker = &orderedClient{fakeDocker: f, log: &log, netRemoveErr: errors.New("network backup_net is in use")}
	deps.Deployer = &orderedDeployer{recordingDeployer: deployer, log: &log}

	if _, err := Up(context.Background(), deps, UpInput{
		Domain:                  "test.example.com",
		CertPath:                certPath,
		KeyPath:                 keyPath,
		ObservabilityAdminEmail: "ops@example.com",
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if indexOf(log, "network-create:backup_net") >= 0 {
		t.Errorf("an in-use backup_net must not be recreated:\n%v", log)
	}
	if got, ok := f.networks["backup_net"]; !ok || got.Name != "backup_net" {
		t.Errorf("in-use backup_net was disturbed: %+v (present=%v)", got, ok)
	}
	if indexOf(log, "deploy:"+string(StackBackup)) < 0 {
		t.Errorf("backup stack was not deployed:\n%v", log)
	}
}

// TestEnsureStackOverlays_OnlyPrivateNetworks pins the data-driven selection:
// only non-external networks are created, named <stack>_<key>.
func TestEnsureStackOverlays_OnlyPrivateNetworks(t *testing.T) {
	f := newFakeDocker()
	compose := []byte(strings.Join([]string{
		"networks:",
		"  net:",
		"    driver: overlay",
		"  traefik-net:",
		"    external: true",
		"services:",
		"  a:",
		"    image: nginx",
		"    networks: [net]",
	}, "\n"))

	created, err := ensureStackOverlays(context.Background(), f, "demo", compose)
	if err != nil {
		t.Fatalf("ensureStackOverlays: %v", err)
	}
	if len(created) != 1 || created[0] != "demo_net" {
		t.Fatalf("created = %v, want [demo_net]", created)
	}
	if _, ok := f.networks["demo_net"]; !ok {
		t.Error("demo_net was not created")
	}
	if _, ok := f.networks["traefik-net"]; ok {
		t.Error("the external traefik-net must not be created here")
	}
	if f.networks["demo_net"].Labels[runtime.StackNamespaceLabel] != "demo" {
		t.Error("demo_net is missing the stack-namespace label")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
