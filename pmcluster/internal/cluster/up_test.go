package cluster

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// newUpDeps returns a fully-wired UpDeps for unit tests.
// It uses a real store + cipher (temp files) and the fake docker + recording deployer.
func newUpDeps(t *testing.T) (UpDeps, *fakeDocker, *recordingDeployer) {
	t.Helper()
	dir := t.TempDir()
	ensureStorageDirs = func(string) error { return nil }

	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	c, err := credentials.Open(filepath.Join(dir, ".encryption_key"))
	if err != nil {
		t.Fatalf("credentials.Open: %v", err)
	}

	f := newFakeDocker()
	f.info = goodSwarmInfo()

	for _, name := range bundledServices {
		f.services[name] = runtime.Service{Name: name, Replicas: 1, Desired: 1}
	}
	deployer := &recordingDeployer{}

	return UpDeps{
		Store:    s,
		Cipher:   c,
		Docker:   f,
		Deployer: deployer,
		Stdout:   io.Discard,
	}, f, deployer
}

// writeTempFile creates a small file with the given content and returns the path.
func writeTempFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
	return path
}

func TestUp_DeploysInCorrectOrder(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, _, deployer := newUpDeps(t)
	in := UpInput{
		Domain:                "test.example.com",
		CertPath:              certPath,
		KeyPath:               keyPath,
		OpenObserveAdminEmail: "ops@example.com",
	}

	res, err := Up(context.Background(), deps, in)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	wantOrder := []string{"infra", "edge", "observability", "backup"}
	if len(deployer.deployedStacks) != len(wantOrder) {
		t.Fatalf("deployed %d stacks, want %d: %v",
			len(deployer.deployedStacks), len(wantOrder), deployer.deployedStacks)
	}
	for i, want := range wantOrder {
		if deployer.deployedStacks[i].Name != want {
			t.Errorf("stack[%d] = %q, want %q", i, deployer.deployedStacks[i].Name, want)
		}
		if deployer.deployedStacks[i].YAMLLen == 0 {
			t.Errorf("stack[%d] %q: YAML length is 0", i, want)
		}
	}
	_ = res
}

func TestUp_IncludesCertKeyAndCredentialSecrets(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, _, _ := newUpDeps(t)
	in := UpInput{
		Domain:                "test.example.com",
		CertPath:              certPath,
		KeyPath:               keyPath,
		OpenObserveAdminEmail: "ops@example.com",
	}

	res, err := Up(context.Background(), deps, in)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	// cert/key are content-addressed now (cert_<sha8>, key_<sha8>) — assert
	// the base names appear with a hash suffix rather than a fixed name.
	hasCert, hasKey := false, false
	for _, s := range res.NewSecrets {
		if strings.HasPrefix(s, "cert_") {
			hasCert = true
		}
		if strings.HasPrefix(s, "key_") {
			hasKey = true
		}
	}
	if !hasCert {
		t.Errorf("NewSecrets missing cert_<hash>; got %v", res.NewSecrets)
	}
	if !hasKey {
		t.Errorf("NewSecrets missing key_<hash>; got %v", res.NewSecrets)
	}
	wantSecrets := map[string]bool{
		"admin_credentials":     false,
		"zo_root_user_password": false,
	}
	for _, s := range res.NewSecrets {
		if _, ok := wantSecrets[s]; ok {
			wantSecrets[s] = true
		}
	}
	for name, seen := range wantSecrets {
		if !seen {
			t.Errorf("NewSecrets missing %q; got %v", name, res.NewSecrets)
		}
	}
}

func TestUp_ValidationError_MissingDomain(t *testing.T) {
	deps, f, deployer := newUpDeps(t)
	in := UpInput{
		Domain:                "",
		CertPath:              "/some/cert",
		KeyPath:               "/some/key",
		OpenObserveAdminEmail: "ops@example.com",
	}

	_, err := Up(context.Background(), deps, in)
	if err == nil {
		t.Fatal("Up: expected error for missing domain, got nil")
	}

	if len(f.networks) > 0 || len(f.secrets) > 0 || len(deployer.deployedStacks) > 0 {
		t.Error("Up touched docker resources despite validation failure")
	}
}

func TestUp_ValidationError_MissingCert(t *testing.T) {
	deps, _, _ := newUpDeps(t)
	_, err := Up(context.Background(), deps, UpInput{
		Domain:                "x.com",
		CertPath:              "",
		KeyPath:               "/key",
		OpenObserveAdminEmail: "ops@x.com",
	})
	if err == nil {
		t.Fatal("expected error for missing CertPath")
	}
}

func TestUp_ValidationError_MissingKey(t *testing.T) {
	deps, _, _ := newUpDeps(t)
	_, err := Up(context.Background(), deps, UpInput{
		Domain:                "x.com",
		CertPath:              "/cert",
		KeyPath:               "",
		OpenObserveAdminEmail: "ops@x.com",
	})
	if err == nil {
		t.Fatal("expected error for missing KeyPath")
	}
}

func TestUp_ValidationError_MissingOpenObserveEmail(t *testing.T) {
	deps, _, _ := newUpDeps(t)
	_, err := Up(context.Background(), deps, UpInput{
		Domain:                "x.com",
		CertPath:              "/cert",
		KeyPath:               "/key",
		OpenObserveAdminEmail: "",
	})
	if err == nil {
		t.Fatal("expected error for missing OpenObserveAdminEmail")
	}
}

func TestUp_PreflightFailure_TouchesNothing(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, f, deployer := newUpDeps(t)

	f.pingErr = errSentinel

	_, err := Up(context.Background(), deps, UpInput{
		Domain:                "test.example.com",
		CertPath:              certPath,
		KeyPath:               keyPath,
		OpenObserveAdminEmail: "ops@example.com",
	})
	if err == nil {
		t.Fatal("Up: expected preflight error, got nil")
	}

	if len(f.networks) > 0 {
		t.Errorf("networks created despite preflight failure: %v", f.networks)
	}
	if len(f.secrets) > 0 {
		t.Errorf("secrets created despite preflight failure: %v", f.secrets)
	}
	if len(deployer.deployedStacks) > 0 {
		t.Errorf("stacks deployed despite preflight failure: %v", deployer.deployedStacks)
	}
}

// TestUp_DefaultStorageNodeIsLeader asserts cluster up records the leader's
// hostname as the default storage_nodes value when none is configured — the
// main node is the storage node by default.
func TestUp_DefaultStorageNodeIsLeader(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, f, _ := newUpDeps(t)
	f.nodes = []runtime.Node{
		{Hostname: "nextrum-sy-1", Role: "manager", IsLeader: true, Status: "ready", Availability: "active"},
		{Hostname: "nextrum-sy-2", Role: "worker", Status: "ready", Availability: "active"},
	}

	if _, err := Up(context.Background(), deps, UpInput{
		Domain:                "test.example.com",
		CertPath:              certPath,
		KeyPath:               keyPath,
		OpenObserveAdminEmail: "ops@example.com",
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	got := deps.Store.GetSettingDefault(context.Background(), SettingStorageNodes(), "")
	if got != "nextrum-sy-1" {
		t.Errorf("default storage_nodes = %q, want the leader hostname nextrum-sy-1", got)
	}
	if len(f.nodeLabels) != 1 || f.nodeLabels[0] != runtime.StorageNodeLabel+"=true" {
		t.Errorf("expected the default storage node to be labeled %s=true, got %v", runtime.StorageNodeLabel, f.nodeLabels)
	}
}

// TestUp_BackupRendersGlobalWhenStorageNodeDefaulted asserts the storage node
// is defaulted BEFORE platform stacks are rendered, so backup_volume-backup is
// deployed global from the first bring-up. Regression for the swarm-e2e
// failure "service mode change is not allowed": defaulting storage_nodes in
// persistInstallState (after deploy) made the first cluster update switch the
// backup agent replicated→global — a mode change Docker rejects in place.
func TestUp_BackupRendersGlobalWhenStorageNodeDefaulted(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, f, deployer := newUpDeps(t)
	f.nodes = []runtime.Node{
		{Hostname: "nextrum-sy-1", Role: "manager", IsLeader: true, Status: "ready", Availability: "active"},
	}

	if _, err := Up(context.Background(), deps, UpInput{
		Domain:                "test.example.com",
		CertPath:              certPath,
		KeyPath:               keyPath,
		OpenObserveAdminEmail: "ops@example.com",
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// storage_nodes must be set (leader hostname) so the render saw it.
	if got := deps.Store.GetSettingDefault(context.Background(), SettingStorageNodes(), ""); got != "nextrum-sy-1" {
		t.Fatalf("default storage_nodes = %q, want nextrum-sy-1 (must be set before render)", got)
	}

	// The backup stack deployed at up time must be global (constrained to the
	// storage node) — never replicated. A subsequent cluster update must then
	// be a content-aware no-op for the backup stack.
	var backupYAML string
	for _, d := range deployer.deployedStacks {
		if d.Name == string(StackBackup) {
			backupYAML = d.YAML
		}
	}
	if backupYAML == "" {
		t.Fatal("backup stack was not deployed during Up")
	}
	if !strings.Contains(backupYAML, "mode: global") {
		t.Errorf("backup stack rendered without mode: global at up time:\n%s", backupYAML)
	}
	if !strings.Contains(backupYAML, runtime.StorageNodeLabel) {
		t.Errorf("backup stack rendered without the storage-node constraint label %s:\n%s", runtime.StorageNodeLabel, backupYAML)
	}
}

// TestUp_StorageNodesPreserved asserts an operator-configured storage_nodes
// list is NOT clobbered by cluster up.
func TestUp_StorageNodesPreserved(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, f, _ := newUpDeps(t)
	f.nodes = []runtime.Node{
		{Hostname: "nextrum-sy-1", Role: "manager", IsLeader: true, Status: "ready", Availability: "active"},
		{Hostname: "nextrum-sy-2", Role: "worker", Status: "ready", Availability: "active"},
	}
	if err := deps.Store.SetSetting(context.Background(), SettingStorageNodes(), "node-a,node-b"); err != nil {
		t.Fatalf("SetSetting storage_nodes: %v", err)
	}

	if _, err := Up(context.Background(), deps, UpInput{
		Domain:                "test.example.com",
		CertPath:              certPath,
		KeyPath:               keyPath,
		OpenObserveAdminEmail: "ops@example.com",
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	got := deps.Store.GetSettingDefault(context.Background(), SettingStorageNodes(), "")
	if got != "node-a,node-b" {
		t.Errorf("storage_nodes clobbered by cluster up: %q, want node-a,node-b", got)
	}
}

// TestUp_DefaultPlatformNodeIsLeader verifies FIX 1: a fresh cluster up pins the
// platform stack to the leader hostname from the start (an empty platform_node
// defaults to the leader), so platform services never fall back to the
// node.role == manager constraint (which constrains nothing on an all-manager
// swarm).
func TestUp_DefaultPlatformNodeIsLeader(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, f, _ := newUpDeps(t)
	f.nodes = []runtime.Node{
		{Hostname: "nextrum-sy-1", Role: "manager", IsLeader: true, Status: "ready", Availability: "active"},
		{Hostname: "nextrum-sy-2", Role: "worker", Status: "ready", Availability: "active"},
	}

	if _, err := Up(context.Background(), deps, UpInput{
		Domain:                "test.example.com",
		CertPath:              certPath,
		KeyPath:               keyPath,
		OpenObserveAdminEmail: "ops@example.com",
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	got := deps.Store.GetSettingDefault(context.Background(), SettingPlatformNode(), "")
	if got != "nextrum-sy-1" {
		t.Errorf("default platform_node = %q, want the leader hostname nextrum-sy-1", got)
	}
}

// TestUp_PlatformNodePreserved verifies FIX 1: an operator-configured
// platform_node is not clobbered by cluster up.
func TestUp_PlatformNodePreserved(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, f, _ := newUpDeps(t)
	f.nodes = []runtime.Node{
		{Hostname: "nextrum-sy-1", Role: "manager", IsLeader: true, Status: "ready", Availability: "active"},
		{Hostname: "nextrum-sy-2", Role: "worker", Status: "ready", Availability: "active"},
	}
	if err := deps.Store.SetSetting(context.Background(), SettingPlatformNode(), "operator-pinned"); err != nil {
		t.Fatalf("SetSetting platform_node: %v", err)
	}

	if _, err := Up(context.Background(), deps, UpInput{
		Domain:                "test.example.com",
		CertPath:              certPath,
		KeyPath:               keyPath,
		OpenObserveAdminEmail: "ops@example.com",
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	got := deps.Store.GetSettingDefault(context.Background(), SettingPlatformNode(), "")
	if got != "operator-pinned" {
		t.Errorf("platform_node clobbered by cluster up: %q, want operator-pinned", got)
	}
}
