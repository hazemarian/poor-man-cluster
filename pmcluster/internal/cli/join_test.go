package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRenderSystemdUnit(t *testing.T) {
	unit := renderSystemdUnit("pmuser", "docker", "/home/pmuser", "/usr/local/bin/pmcluster serve")
	for _, want := range []string{
		"[Unit]",
		"Description=pmcluster API Server",
		"After=docker.service",
		"BindsTo=docker.service",
		"[Service]",
		"Type=simple",
		"User=pmuser",
		"Group=docker",
		"Environment=HOME=/home/pmuser",
		"ExecStart=/usr/local/bin/pmcluster serve",
		"Restart=always",
		"RestartSec=5",
		"[Install]",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit template missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "${") {
		t.Errorf("unit template contains unresolved variables:\n%s", unit)
	}
}

// Regression: the systemd unit must run `pmcluster serve`, never the bare
// binary. A bare ExecStart prints help and exits, crash-looping the service.
func TestDaemonExecStart_IncludesServe(t *testing.T) {
	if got := daemonExecStart("/usr/local/bin/pmcluster"); got != "/usr/local/bin/pmcluster serve" {
		t.Fatalf("daemonExecStart(/usr/local/bin/pmcluster) = %q, want %q", got, "/usr/local/bin/pmcluster serve")
	}
}

func TestJoinCommandRegistration(t *testing.T) {
	if joinCmd == nil {
		t.Fatal("joinCmd is nil")
	}
	if joinCmd.Use != "join" {
		t.Errorf("expected Use=join, got %q", joinCmd.Use)
	}
	if joinCmd.Short == "" {
		t.Error("joinCmd has no Short description")
	}
	// --token and --manager flags must exist.
	var hasToken, hasManager, hasHostname bool
	for _, f := range []string{"token", "manager", "hostname"} {
		if joinCmd.Flags().Lookup(f) != nil {
			switch f {
			case "token":
				hasToken = true
			case "manager":
				hasManager = true
			case "hostname":
				hasHostname = true
			}
		}
	}
	if !hasToken || !hasManager || !hasHostname {
		t.Errorf("joinCmd missing --token (hasToken=%v), --manager (hasManager=%v), or --hostname (hasHostname=%v)", hasToken, hasManager, hasHostname)
	}
}

// TestSetNodeHostname_RejectsGarbage asserts the hostname value is validated
// before it reaches hostnamectl (and later lands verbatim in Swarm constraint
// expressions and the node list match).
func TestSetNodeHostname_RejectsGarbage(t *testing.T) {
	for _, bad := range []string{"bad host!", "has/slash", "quote\"d", "a b", "$(rm)", "new\nline"} {
		if err := setNodeHostname(t.Context(), io.Discard, bad); err == nil {
			t.Errorf("expected error for hostname %q, got nil", bad)
		}
	}
}

func TestJoinRoleFlagValidated(t *testing.T) {
	// --role must be worker or manager.
	for _, bad := range []string{"master", "", "swarm"} {
		c := setupJoinTestCmd()
		c.Flags().Set("role", bad)
		c.Flags().Set("token", "SWMTKN-1-x")
		c.Flags().Set("manager", "10.0.0.5:2377")
		err := runJoin(c, nil)
		if err == nil {
			t.Fatalf("expected error for --role %q, got nil", bad)
		}
		if !strings.Contains(err.Error(), "--role must be 'worker' or 'manager'") {
			t.Fatalf("unexpected error for --role %q: %v", bad, err)
		}
	}
}

func TestJoinRequiresTokenAndManager(t *testing.T) {
	c := setupJoinTestCmd()
	c.Flags().Set("role", "worker")
	c.Flags().Set("token", "")
	c.Flags().Set("manager", "10.0.0.5:2377")
	if err := runJoin(c, nil); err == nil || !strings.Contains(err.Error(), "--token") {
		t.Fatalf("expected --token error, got %v", err)
	}
	c = setupJoinTestCmd()
	c.Flags().Set("role", "worker")
	c.Flags().Set("token", "SWMTKN-1-x")
	c.Flags().Set("manager", "")
	if err := runJoin(c, nil); err == nil || !strings.Contains(err.Error(), "--manager") {
		t.Fatalf("expected --manager error, got %v", err)
	}
}

// setupJoinTestCmd returns a joinCmd clone whose flags are reset.
func setupJoinTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "join"}
	c.Flags().String("token", "", "")
	c.Flags().String("manager", "", "")
	c.Flags().String("role", "worker", "")
	c.Flags().String("copy-registry-creds", "", "")
	c.Flags().String("verify-registry-pull", "", "")
	return c
}

// --- Q2: registry credentials on join ---------------------------------------

// TestJoinCommandRegistration_RegistryFlags asserts both optional registry
// flags are registered on joinCmd (they are purely opt-in: absent = today's
// warn-only behaviour).
func TestJoinCommandRegistration_RegistryFlags(t *testing.T) {
	if joinCmd == nil {
		t.Fatal("joinCmd is nil")
	}
	copyF := joinCmd.Flags().Lookup("copy-registry-creds")
	if copyF == nil {
		t.Fatal("joinCmd missing --copy-registry-creds")
		return
	}
	if copyF.DefValue != "" {
		t.Errorf("--copy-registry-creds default = %q, want empty (opt-in)", copyF.DefValue)
	}
	pullF := joinCmd.Flags().Lookup("verify-registry-pull")
	if pullF == nil {
		t.Fatal("joinCmd missing --verify-registry-pull")
		return
	}
	if pullF.DefValue != "" {
		t.Errorf("--verify-registry-pull default = %q, want empty (opt-in)", pullF.DefValue)
	}
	// And both must still be settable/gettable as strings.
	if err := joinCmd.Flags().Set("copy-registry-creds", "10.0.0.5"); err != nil {
		t.Errorf("set --copy-registry-creds: %v", err)
	}
	if err := joinCmd.Flags().Set("verify-registry-pull", "ghcr.io/acme/app:1"); err != nil {
		t.Errorf("set --verify-registry-pull: %v", err)
	}
	if got, _ := joinCmd.Flags().GetString("copy-registry-creds"); got != "10.0.0.5" {
		t.Errorf("GetString(copy-registry-creds) = %q, want 10.0.0.5", got)
	}
	if got, _ := joinCmd.Flags().GetString("verify-registry-pull"); got != "ghcr.io/acme/app:1" {
		t.Errorf("GetString(verify-registry-pull) = %q, want ghcr.io/acme/app:1", got)
	}
	// Reset so the package-level command isn't left dirty for other tests.
	joinCmd.Flags().Set("copy-registry-creds", "")
	joinCmd.Flags().Set("verify-registry-pull", "")
}

// swapDockerConfigPath points dockerConfigPathFn at a temp dir and returns a
// restore func (daemon_test.go-style package var overrides).
func swapDockerConfigPath(t *testing.T) (string, func()) {
	t.Helper()
	old := dockerConfigPathFn
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	dockerConfigPathFn = func() (string, error) { return cfgPath, nil }
	return cfgPath, func() { dockerConfigPathFn = old }
}

// TestCopyRegistryCreds_MergesAndBacksUp covers the fresh-copy path: ssh is
// faked, the manager's auths land in the local config, an existing local
// config is MERGED (never clobbered) and backed up to config.json.bak.
func TestCopyRegistryCreds_MergesAndBacksUp(t *testing.T) {
	cfgPath, restore := swapDockerConfigPath(t)
	defer restore()

	// Pre-existing local credentials for a different registry + a credsStore
	// key: both must survive the copy.
	local := `{"auths":{"local.example.com":{"auth":"bG9jYWw6c2VjcmV0"}},"credsStore":"desktop"}`
	if err := os.WriteFile(cfgPath, []byte(local), 0o600); err != nil {
		t.Fatalf("seed local config: %v", err)
	}

	oldSSH := sshRemoteCatFn
	defer func() { sshRemoteCatFn = oldSSH }()
	remote := `{"auths":{"ghcr.io":{"auth":"Z2hjcmY6dG9rZW4="}}}`
	var gotHost, gotPath string
	sshRemoteCatFn = func(_ context.Context, host, remotePath string) ([]byte, error) {
		gotHost, gotPath = host, remotePath
		return []byte(remote), nil
	}

	var out bytes.Buffer
	if !copyRegistryCreds(t.Context(), &out, "10.0.0.5") {
		t.Fatalf("copyRegistryCreds = false, want true; output:\n%s", out.String())
	}
	if gotHost != "10.0.0.5" || gotPath != "/root/.docker/config.json" {
		t.Errorf("ssh called with host=%q path=%q, want 10.0.0.5 + /root/.docker/config.json", gotHost, gotPath)
	}
	if !strings.Contains(out.String(), "✔ registry credentials copied from 10.0.0.5") {
		t.Errorf("output missing success line:\n%s", out.String())
	}

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read merged config: %v", err)
	}
	for _, want := range []string{
		"ghcr.io",           // copied registry survived
		"local.example.com", // pre-existing registry NOT clobbered
		"credsStore",        // non-auth local key preserved
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("merged config missing %q:\n%s", want, got)
		}
	}
	// It must be valid JSON with both auths entries.
	bak, err := os.ReadFile(cfgPath + ".bak")
	if err != nil {
		t.Fatalf("backup file not created: %v", err)
	}
	if string(bak) != local {
		t.Errorf("backup = %q, want the original local config %q", bak, local)
	}
}

// TestCopyRegistryCreds_FreshCopy asserts that when no local config exists the
// file is written outright (no .bak) and the exact remote bytes are installed.
func TestCopyRegistryCreds_FreshCopy(t *testing.T) {
	cfgPath, restore := swapDockerConfigPath(t)
	defer restore()

	oldSSH := sshRemoteCatFn
	defer func() { sshRemoteCatFn = oldSSH }()
	remote := `{"auths":{"ghcr.io":{"auth":"dXNlcjpwYXNz"}}}`
	sshRemoteCatFn = func(context.Context, string, string) ([]byte, error) {
		return []byte(remote + "\n"), nil // trailing newline like a real ssh cat
	}

	var out bytes.Buffer
	if !copyRegistryCreds(t.Context(), &out, "manager.example.com") {
		t.Fatalf("copyRegistryCreds = false, want true; output:\n%s", out.String())
	}
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read copied config: %v", err)
	}
	var parsed struct {
		Auths map[string]any `json:"auths"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("copied config is not valid JSON: %v\n%s", err, got)
	}
	if _, ok := parsed.Auths["ghcr.io"]; !ok {
		t.Errorf("copied config missing ghcr.io auth: %s", got)
	}
	if _, err := os.Stat(cfgPath + ".bak"); !os.IsNotExist(err) {
		t.Errorf("unexpected backup file on a fresh copy (err=%v)", err)
	}
	if !strings.Contains(out.String(), "✔ registry credentials copied from manager.example.com") {
		t.Errorf("output missing success line:\n%s", out.String())
	}
}

// TestCopyRegistryCreds_FailureIsNonFatal asserts every failure mode only
// warns and returns false — the join itself must keep going.
func TestCopyRegistryCreds_FailureIsNonFatal(t *testing.T) {
	oldSSH := sshRemoteCatFn
	oldCopyTimeout := registryCopyTimeout
	defer func() {
		sshRemoteCatFn = oldSSH
		registryCopyTimeout = oldCopyTimeout
	}()

	cases := []struct {
		name string
		host string
		ssh  func(context.Context, string, string) ([]byte, error)
	}{
		{"empty host", "", func(context.Context, string, string) ([]byte, error) { return []byte("{}"), nil }},
		{"ssh error", "10.0.0.5", func(context.Context, string, string) ([]byte, error) {
			return nil, errors.New("ssh: connect to host 10.0.0.5 port 22: Connection refused")
		}},
		{"empty output", "10.0.0.5", func(context.Context, string, string) ([]byte, error) { return []byte("  \n"), nil }},
		{"non-json output", "10.0.0.5", func(context.Context, string, string) ([]byte, error) {
			return []byte("bash: /root/.docker/config.json: No such file or directory"), nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sshRemoteCatFn = tc.ssh
			cfgPath, restore := swapDockerConfigPath(t)
			defer restore()
			var out bytes.Buffer
			if copyRegistryCreds(t.Context(), &out, tc.host) {
				t.Fatalf("copyRegistryCreds = true, want false; output:\n%s", out.String())
			}
			if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
				t.Errorf("config file written despite failure (err=%v)", err)
			}
			if tc.host != "" && !strings.Contains(out.String(), "⚠") {
				t.Errorf("expected a warning in output:\n%s", out.String())
			}
		})
	}
}

// TestVerifyRegistryPull_FailureWarnsOnly asserts a failed 'docker pull' is
// reported as a warning and never surfaces as an error (join must succeed).
func TestVerifyRegistryPull_FailureWarnsOnly(t *testing.T) {
	oldPull := dockerPullFn
	oldTimeout := registryPullTimeout
	defer func() {
		dockerPullFn = oldPull
		registryPullTimeout = oldTimeout
	}()
	registryPullTimeout = 0 // prove the timeout wrapper never hangs a test

	var gotImage string
	dockerPullFn = func(_ context.Context, image string) error {
		gotImage = image
		return errors.New("Error response from daemon: unauthorized: authentication required")
	}

	var out bytes.Buffer
	// No return value — by construction the pull cannot fail the join.
	verifyRegistryPull(t.Context(), &out, "ghcr.io/acme/app:1.2.3")
	if gotImage != "ghcr.io/acme/app:1.2.3" {
		t.Errorf("docker pull called with %q, want ghcr.io/acme/app:1.2.3", gotImage)
	}
	s := out.String()
	if !strings.Contains(s, "registry pull failed — check credentials") {
		t.Errorf("output missing failure warning:\n%s", s)
	}
	if !strings.Contains(s, "⚠") {
		t.Errorf("expected a warning marker in output:\n%s", s)
	}
}

func TestVerifyRegistryPull_SuccessAndEmpty(t *testing.T) {
	oldPull := dockerPullFn
	defer func() { dockerPullFn = oldPull }()

	dockerPullFn = func(context.Context, string) error { return nil }
	var out bytes.Buffer
	verifyRegistryPull(t.Context(), &out, "ghcr.io/acme/app:1")
	if !strings.Contains(out.String(), "✔ registry pull succeeded") {
		t.Errorf("output missing success line:\n%s", out.String())
	}

	// Empty image = flag not set → must not shell out at all.
	called := false
	dockerPullFn = func(context.Context, string) error { called = true; return nil }
	out.Reset()
	verifyRegistryPull(t.Context(), &out, "  ")
	if called {
		t.Error("docker pull invoked for an empty --verify-registry-pull value")
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for empty image, got %q", out.String())
	}
}

// TestMergeDockerConfigs unit-checks the merge rules directly.
func TestMergeDockerConfigs(t *testing.T) {
	local := `{"auths":{"a.example.com":{"auth":"bG9jYWw="}},"credsStore":"desktop"}`
	remote := `{"auths":{"a.example.com":{"auth":"bmV3ZXI="},"b.example.com":{"auth":"bmV3"}}}`
	merged, err := mergeDockerConfigs([]byte(local), []byte(remote))
	if err != nil {
		t.Fatalf("mergeDockerConfigs: %v", err)
	}
	var got struct {
		Auths      map[string]map[string]string `json:"auths"`
		CredsStore string                       `json:"credsStore"`
	}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("merged config invalid JSON: %v\n%s", err, merged)
	}
	if got.Auths["a.example.com"]["auth"] != "bmV3ZXI=" {
		t.Errorf("remote auth should win per-registry, got %q", got.Auths["a.example.com"]["auth"])
	}
	if _, ok := got.Auths["b.example.com"]; !ok {
		t.Errorf("remote-only registry missing: %v", got.Auths)
	}
	if got.CredsStore != "desktop" {
		t.Errorf("credsStore = %q, want desktop", got.CredsStore)
	}
}

// --- Path 1: --storage-node registration -------------------------------------

// TestAppendStorageNode covers the pure list-merge used by registerStorageNode:
// trimming, dedup, empty-entry dropping, and the "already present" no-op.
func TestAppendStorageNode(t *testing.T) {
	cases := []struct {
		name    string
		current string
		host    string
		want    string
		added   bool
	}{
		{"empty current", "", "node-2", "node-2", true},
		{"single existing", "node-1", "node-2", "node-1,node-2", true},
		{"already present", "node-1", "node-1", "node-1", false},
		{"duplicate in current", "node-1,node-1", "node-2", "node-1,node-2", true},
		{"whitespace entries", "node-1, ,node-2", "node-3", "node-1,node-2,node-3", true},
		{"already present with spaces", "node-1, node-2", "node-2", "node-1,node-2", false},
		{"empty host", "node-1", "  ", "node-1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, added := appendStorageNode(tc.current, tc.host)
			if got != tc.want || added != tc.added {
				t.Errorf("appendStorageNode(%q, %q) = (%q, %v), want (%q, %v)",
					tc.current, tc.host, got, added, tc.want, tc.added)
			}
		})
	}
}

// TestRegisterStorageNode_AppendsAndSkips exercises the ssh-faked happy path
// (node appended to storage_nodes) and the already-registered no-op. The
// failure path (ssh errors) only warns — the join itself must keep going.
func TestRegisterStorageNode_AppendsAndSkips(t *testing.T) {
	oldSSH := sshRunFn
	oldTimeout := registryCopyTimeout
	defer func() {
		sshRunFn = oldSSH
		registryCopyTimeout = oldTimeout
	}()
	registryCopyTimeout = 0 // never hang the test

	t.Run("appends new node", func(t *testing.T) {
		var calls []string
		sshRunFn = func(_ context.Context, host, remoteCmd string) ([]byte, error) {
			calls = append(calls, host+": "+remoteCmd)
			if strings.Contains(remoteCmd, "get") {
				return []byte("storage_nodes=node-1\n"), nil
			}
			return []byte("ok"), nil
		}
		var out bytes.Buffer
		registerStorageNode(t.Context(), &out, "10.0.0.5", "node-2")
		s := out.String()
		if !strings.Contains(s, `"node-2" registered as a storage node (storage_nodes=node-1,node-2)`) {
			t.Errorf("output missing success line:\n%s", s)
		}
		if len(calls) != 3 {
			t.Errorf("expected 3 ssh calls (get + set + label), got %d: %v", len(calls), calls)
		}
		if !strings.Contains(calls[2], "docker node update --label-add pmcluster.storage=true node-2") {
			t.Errorf("expected the storage label command as the 3rd ssh call, got: %v", calls[2])
		}
	})

	t.Run("already registered", func(t *testing.T) {
		var calls []string
		sshRunFn = func(_ context.Context, host, remoteCmd string) ([]byte, error) {
			calls = append(calls, remoteCmd)
			return []byte("storage_nodes=node-1,node-2\n"), nil
		}
		var out bytes.Buffer
		registerStorageNode(t.Context(), &out, "10.0.0.5", "node-2")
		s := out.String()
		if !strings.Contains(s, `"node-2" is already a storage node`) {
			t.Errorf("output missing already-registered line:\n%s", s)
		}
		if len(calls) != 1 {
			t.Errorf("expected 1 ssh call (get only), got %d: %v", len(calls), calls)
		}
	})

	t.Run("ssh failure warns with manual hint", func(t *testing.T) {
		var calls []string
		sshRunFn = func(_ context.Context, host, remoteCmd string) ([]byte, error) {
			calls = append(calls, remoteCmd)
			return nil, errors.New("ssh: Connection refused")
		}
		var out bytes.Buffer
		registerStorageNode(t.Context(), &out, "10.0.0.5", "node-2")
		s := out.String()
		if !strings.Contains(s, "⚠") {
			t.Errorf("expected a warning in output:\n%s", s)
		}
		if !strings.Contains(s, "pmcluster cluster settings set storage_nodes=") {
			t.Errorf("output missing manual fallback command:\n%s", s)
		}
	})

	t.Run("empty ssh host warns", func(t *testing.T) {
		sshRunFn = func(context.Context, string, string) ([]byte, error) {
			t.Error("sshRunFn must not be called with an empty host")
			return nil, nil
		}
		var out bytes.Buffer
		registerStorageNode(t.Context(), &out, "", "node-2")
		if !strings.Contains(out.String(), "⚠") {
			t.Errorf("expected a warning in output:\n%s", out.String())
		}
	})
}

// TestJoinCommandRegistration_StorageNodeFlag asserts --storage-node is
// registered (opt-in, default false) and settable.
func TestJoinCommandRegistration_StorageNodeFlag(t *testing.T) {
	if joinCmd == nil {
		t.Fatal("joinCmd is nil")
	}
	f := joinCmd.Flags().Lookup("storage-node")
	if f == nil {
		t.Fatal("joinCmd missing --storage-node")
		return
	}
	if f.DefValue != "false" {
		t.Errorf("--storage-node default = %q, want false", f.DefValue)
	}
	if err := joinCmd.Flags().Set("storage-node", "true"); err != nil {
		t.Errorf("set --storage-node: %v", err)
	}
	if got, _ := joinCmd.Flags().GetBool("storage-node"); !got {
		t.Error("GetBool(storage-node) = false, want true")
	}
	joinCmd.Flags().Set("storage-node", "false")
}
