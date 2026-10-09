package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/stacks"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// restoreDemoManifest is a minimal stateful manifest used to seed a stack
// revision so StoragePinsForStack can resolve a pinned owner.
const restoreDemoManifest = `
app: sfapp
env: production
domain: example.com
services:
  db:
    image: postgres:14-alpine
    volumes: [db_data:/var/lib/postgresql/data]
`

// seedRestoreStack records a stateful revision for `stack` and, when node is
// non-empty, pins it there (stack_pin_<stack>).
func seedRestoreStack(t *testing.T, st *store.Store, stack, node string) {
	t.Helper()
	ctx := context.Background()
	if err := st.RecordDeploy(ctx, &store.StackRevision{
		StackName:  stack,
		Revision:   time.Now().UnixNano(),
		SourceYAML: restoreDemoManifest,
	}, ""); err != nil {
		t.Fatalf("RecordDeploy: %v", err)
	}
	if node != "" {
		if err := st.SetSetting(ctx, stacks.StackPinKey(stack), node); err != nil {
			t.Fatalf("SetSetting: %v", err)
		}
	}
}

// seedRestoreBackup records a succeeded backup row whose (bare) archive path is
// `key`, so restoreArchiveKey resolves filepath.Base(key).
func seedRestoreBackup(t *testing.T, st *store.Store, key string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.CreateBackup(ctx, "", 0)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if err := st.FinishBackup(ctx, id, "succeeded", key, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}
	return id
}

// seedInClusterStore inserts a seaweedfs_admin credential (encrypted with the
// data dir's key) so backupS3Config prefers the in-cluster store.
func seedInClusterStore(t *testing.T, cfg *config.Config) {
	t.Helper()
	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		t.Fatalf("credentials.Open: %v", err)
	}
	pass, err := cipher.Encrypt([]byte("seaweed-secret"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.InsertCredential(context.Background(), &store.ManagedCredential{
		Name:               "seaweedfs_admin",
		Kind:               "basic",
		Username:           "admin",
		PasswordCiphertext: pass,
	}); err != nil {
		t.Fatalf("InsertCredential: %v", err)
	}
}

// fakeDockerBinary installs a fake `docker` script first on PATH. It records
// the `service create` argv to `capture` (and waits for that file in `service
// inspect` so the create argv is flushed before the mover proceeds), then
// reports the one-shot task Complete.
func fakeDockerBinary(t *testing.T, capture string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = service ] && [ "$2" = create ]; then
  printf '%%s\n' "$@" > %q
  exit 0
fi
if [ "$1" = service ] && [ "$2" = inspect ]; then
  i=0
  while [ ! -f %q ] && [ $i -lt 500 ]; do sleep 0.01; i=$((i+1)); done
  exit 0
fi
if [ "$1" = service ] && [ "$2" = ps ]; then
  echo "Complete"
  exit 0
fi
exit 0
`, capture, capture)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestStackFromVolume(t *testing.T) {
	cases := map[string]string{
		"sfapp/db_data": "sfapp",
		"db_data":       "",
		"":              "",
		"/db_data":      "",
		"a/b/c":         "a",
	}
	for in, want := range cases {
		if got := stackFromVolume(in); got != want {
			t.Errorf("stackFromVolume(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRestoreDestination pins the BUG-032 decision matrix: a local owner (or
// no owner, or no Docker) keeps the local restore; a remote owner routes via
// the store; a remote owner without a store fails loudly.
func TestRestoreDestination(t *testing.T) {
	cases := []struct {
		name      string
		owner     string
		localHost string
		dockerOK  bool
		storeOK   bool
		wantNode  string
		wantErr   string
	}{
		{name: "owner is local", owner: "node-a", localHost: "node-a", dockerOK: true, storeOK: true, wantNode: ""},
		{name: "no node-pinned owner", owner: "", localHost: "node-a", dockerOK: true, storeOK: true, wantNode: ""},
		{name: "remote owner routes via store", owner: "node-b", localHost: "node-a", dockerOK: true, storeOK: true, wantNode: "node-b"},
		{name: "remote owner without store fails", owner: "node-b", localHost: "node-a", dockerOK: true, storeOK: false, wantErr: "this stack's volume lives on node-b"},
		{name: "remote owner without docker keeps local", owner: "node-b", localHost: "node-a", dockerOK: false, storeOK: true, wantNode: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node, err := restoreDestination(tc.owner, tc.localHost, tc.dockerOK, tc.storeOK)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if node != tc.wantNode {
				t.Fatalf("node = %q, want %q", node, tc.wantNode)
			}
		})
	}
}

// restoreTestCmd builds a cobra command with a context and output, wired to the
// package configPath via newTestCLIEnv.
func restoreTestCmd(t *testing.T) (*cobra.Command, func()) {
	t.Helper()
	_, restore := newTestCLIEnv(t)
	cmd := &cobra.Command{Use: "restore"}
	cmd.SetContext(context.Background())
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, restore
}

// TestRestoreRouteForStack exercises the CLI's routing end to end: local owner
// → local, remote owner without a store → loud failure, remote owner with a
// store → routed onto the owner (and the mover argv names the owner node).
func TestRestoreRouteForStack(t *testing.T) {
	local := localHostname()

	t.Run("local owner stays local", func(t *testing.T) {
		cmd, restore := restoreTestCmd(t)
		defer restore()
		st, _, err := openStore()
		if err != nil {
			t.Fatalf("openStore: %v", err)
		}
		seedRestoreStack(t, st, "sfapp", local)
		_ = st.Close()

		target, err := restoreRouteForStack(cmd, 0, "sfapp", false)
		if err != nil {
			t.Fatalf("restoreRouteForStack: %v", err)
		}
		if target != "" {
			t.Fatalf("target = %q, want empty (local restore)", target)
		}
	})

	t.Run("remote owner without store fails loudly", func(t *testing.T) {
		cmd, restore := restoreTestCmd(t)
		defer restore()
		st, _, err := openStore()
		if err != nil {
			t.Fatalf("openStore: %v", err)
		}
		seedRestoreStack(t, st, "sfapp", "nxt-sw-2-m")
		_ = st.Close()

		_, err = restoreRouteForStack(cmd, 0, "sfapp", false)
		if err == nil || !strings.Contains(err.Error(), "this stack's volume lives on nxt-sw-2-m") {
			t.Fatalf("err = %v, want the loud lives-on error", err)
		}
	})

	t.Run("remote owner routes via store mover", func(t *testing.T) {
		cmd, restore := restoreTestCmd(t)
		defer restore()

		capture := filepath.Join(t.TempDir(), "argv.txt")
		fakeDockerBinary(t, capture)

		// Seed the in-cluster store (seaweedfs_admin) so storeOK is true.
		cfg, _ := config.Load(configPath)
		if cfg == nil {
			t.Fatal("config.Load returned nil")
		}
		seedInClusterStore(t, cfg)

		st, _, err := openStore()
		if err != nil {
			t.Fatalf("openStore: %v", err)
		}
		seedRestoreStack(t, st, "sfapp", "nxt-sw-2-m")
		id := seedRestoreBackup(t, st, "backup-node1-2026-10-08T10-00-00.tar.gz")
		_ = st.Close()

		target, err := restoreRouteForStack(cmd, id, "sfapp", false)
		if err != nil {
			t.Fatalf("restoreRouteForStack: %v", err)
		}
		if target != "nxt-sw-2-m" {
			t.Fatalf("target = %q, want nxt-sw-2-m", target)
		}

		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatalf("read captured argv: %v", err)
		}
		joined := strings.Join(strings.Fields(string(data)), " ")
		for _, want := range []string{
			"--constraint node.hostname==nxt-sw-2-m",
			"--host host.docker.internal:host-gateway",
			"RCLONE_CONFIG_SW_ENDPOINT=http://host.docker.internal:8333",
			"rclone copyto sw:pmcluster-backups/backup-node1-2026-10-08T10-00-00.tar.gz /tmp/m.tgz",
			"the archive does not contain backup/data/sfapp",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("mover argv missing %q:\n%s", want, joined)
			}
		}
	})

	t.Run("remote owner routes from-s3 via offsite mover", func(t *testing.T) {
		cmd, restore := restoreTestCmd(t)
		defer restore()

		capture := filepath.Join(t.TempDir(), "argv.txt")
		fakeDockerBinary(t, capture)

		// Seed BOTH the in-cluster store and the offsite destination.
		cfg, _ := config.Load(configPath)
		if cfg == nil {
			t.Fatal("config.Load returned nil")
		}
		seedInClusterStore(t, cfg)

		st, _, err := openStore()
		if err != nil {
			t.Fatalf("openStore: %v", err)
		}
		ctx := context.Background()
		for k, v := range map[string]string{
			cluster.SettingBackupS3Endpoint():  "https://r2.example.com",
			cluster.SettingBackupS3Bucket():    "offsite-bucket",
			cluster.SettingBackupS3AccessKey(): "offak",
			cluster.SettingBackupS3SecretKey(): "offsk",
			cluster.SettingBackupS3Region():    "auto",
		} {
			if err := st.SetSetting(ctx, k, v); err != nil {
				t.Fatalf("SetSetting %s: %v", k, err)
			}
		}
		seedRestoreStack(t, st, "sfapp", "nxt-sw-2-m")
		id := seedRestoreBackup(t, st, "backup-node1-2026-10-08T10-00-00.tar.gz")
		_ = st.Close()

		target, err := restoreRouteForStack(cmd, id, "sfapp", true)
		if err != nil {
			t.Fatalf("restoreRouteForStack: %v", err)
		}
		if target != "nxt-sw-2-m" {
			t.Fatalf("target = %q, want nxt-sw-2-m", target)
		}

		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatalf("read captured argv: %v", err)
		}
		joined := strings.Join(strings.Fields(string(data)), " ")
		for _, want := range []string{
			"RCLONE_CONFIG_SW_ENDPOINT=https://r2.example.com",
			"RCLONE_CONFIG_SW_ACCESS_KEY_ID=offak",
			"RCLONE_CONFIG_SW_SECRET_ACCESS_KEY=offsk",
			"rclone copyto sw:offsite-bucket/backup-node1-2026-10-08T10-00-00.tar.gz /tmp/m.tgz",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("mover argv missing %q:\n%s", want, joined)
			}
		}
		if strings.Contains(joined, "127.0.0.1") || strings.Contains(joined, "pmcluster-backups") {
			t.Errorf("from-s3 restore must not use the in-cluster store:\n%s", joined)
		}
	})
}
