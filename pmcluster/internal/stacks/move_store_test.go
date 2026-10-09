package stacks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
)

// TestStorePullMoverScriptAndArgs pins the shape of the store-based mover: an
// rclone+tar one-shot that pulls the archive object and unpacks the stack's
// subtree into the mounted volume root, with no cross-node HTTP at all.
func TestStorePullMoverScriptAndArgs(t *testing.T) {
	script := storePullMoverScript("pmcluster-backups", "backup-node-2026-10-08T10-00-00.tar.gz", "sfapp")
	for _, want := range []string{
		"rclone copyto sw:pmcluster-backups/backup-node-2026-10-08T10-00-00.tar.gz /tmp/m.tgz",
		"tar -xzf /tmp/m.tgz -C /tmp/x",
		"rm -rf /data/sfapp",
		"mv /tmp/x/backup/data/sfapp /data/",
		"the archive does not contain backup/data/sfapp",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// BUG-031: the empty/wrong-archive guard must run BEFORE the target's
	// subtree is wiped — otherwise a bad archive would erase /data/sfapp.
	guardIdx := strings.Index(script, "the archive does not contain")
	rmIdx := strings.Index(script, "rm -rf /data/sfapp")
	if guardIdx == -1 || rmIdx == -1 || guardIdx >= rmIdx {
		t.Errorf("the archive-content guard must precede rm -rf /data/sfapp:\n%s", script)
	}

	env := rcloneStoreEnv(backups.S3Config{Endpoint: "http://127.0.0.1:8333", Bucket: "pmcluster-backups", AccessKey: "ak", SecretKey: "sk"})
	args := storePullMoverArgs("pmcluster-move-sfapp-abcdef12", "/var/stack/data", "nxt-sw-4-m", env, script)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--name pmcluster-move-sfapp-abcdef12",
		"--constraint node.hostname==nxt-sw-4-m",
		"--restart-condition none",
		"--mount type=bind,source=/var/stack/data,destination=/data",
		"--entrypoint /bin/sh",
		"RCLONE_CONFIG_SW_ENDPOINT=http://127.0.0.1:8333",
		"RCLONE_CONFIG_SW_ACCESS_KEY_ID=ak",
		"RCLONE_CONFIG_SW_SECRET_ACCESS_KEY=sk",
		"RCLONE_CONFIG_SW_FORCE_PATH_STYLE=true",
		storePullMoverImage,
		"-c set -e;",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("mover args missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(strings.Join(args, " "), "--host host.docker.internal:host-gateway") {
		t.Fatalf("mover args must map host.docker.internal: %v", args)
	}
}

// TestRcloneStoreEnv_EndpointScheme: a scheme-less endpoint (the offsite S3
// form) defaults to https, an existing scheme is preserved, and an empty
// region defaults (SeaweedFS echoes any region).
func TestRcloneStoreEnv_EndpointScheme(t *testing.T) {
	e := strings.Join(rcloneStoreEnv(backups.S3Config{Endpoint: "s3.eu-central-3.ionoscloud.com", Region: "eu-central-3"}), " ")
	if !strings.Contains(e, "RCLONE_CONFIG_SW_ENDPOINT=https://s3.eu-central-3.ionoscloud.com") {
		t.Errorf("scheme-less endpoint must default to https: %s", e)
	}
	if !strings.Contains(e, "RCLONE_CONFIG_SW_REGION=eu-central-3") {
		t.Errorf("region lost: %s", e)
	}
	e2 := strings.Join(rcloneStoreEnv(backups.S3Config{Endpoint: "http://127.0.0.1:8333"}), " ")
	if !strings.Contains(e2, "RCLONE_CONFIG_SW_ENDPOINT=http://127.0.0.1:8333") {
		t.Errorf("existing scheme must be preserved: %s", e2)
	}
	if !strings.Contains(e2, "RCLONE_CONFIG_SW_REGION=us-east-1") {
		t.Errorf("empty region must default: %s", e2)
	}
}

// TestNewestArchiveObject picks the newest whole-disk archive for a given
// source node and never a control-plane archive or a non-tarball.
func TestNewestArchiveObject(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>pmcluster-backups</Name><KeyCount>4</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>
  <Contents><Key>backup-node1-2026-10-08T09-00-00.tar.gz</Key><LastModified>2026-10-08T09:00:01.000Z</LastModified><Size>1</Size></Contents>
  <Contents><Key>backup-node1-2026-10-08T10-00-00.tar.gz</Key><LastModified>2026-10-08T10:00:01.000Z</LastModified><Size>1</Size></Contents>
  <Contents><Key>pmcluster-ctlplane-node1-2026-10-08T11-00-00.tar.gz</Key><LastModified>2026-10-08T11:00:01.000Z</LastModified><Size>1</Size></Contents>
  <Contents><Key>notes.txt</Key><LastModified>2026-10-08T12:00:01.000Z</LastModified><Size>1</Size></Contents>
</ListBucketResult>`

	newSvc := func(tt *testing.T) *Service {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("prefix"); got != "backup-" {
				tt.Errorf("prefix = %q, want backup-", got)
			}
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, body)
		}))
		tt.Cleanup(srv.Close)
		return &Service{S3: backups.S3Config{Endpoint: srv.URL, Bucket: "pmcluster-backups", AccessKey: "ak", SecretKey: "sk"}}
	}

	t.Run("source node filter", func(t *testing.T) {
		svc := newSvc(t)
		key, err := svc.newestArchiveObject(context.Background(), "sfapp", "node1")
		if err != nil {
			t.Fatalf("newestArchiveObject: %v", err)
		}
		if key != "backup-node1-2026-10-08T10-00-00.tar.gz" {
			t.Fatalf("key = %q, want the newest backup-*.tar.gz for node1 (ctlplane + non-tarball excluded)", key)
		}
	})

	t.Run("source node mismatch", func(t *testing.T) {
		svc := newSvc(t)
		_, err := svc.newestArchiveObject(context.Background(), "sfapp", "node2")
		if err == nil || !strings.Contains(err.Error(), "no archive from node") {
			t.Fatalf("err = %v, want a no-archive-from-node error", err)
		}
	})

	t.Run("empty source node picks newest overall", func(t *testing.T) {
		svc := newSvc(t)
		key, err := svc.newestArchiveObject(context.Background(), "sfapp", "")
		if err != nil {
			t.Fatalf("newestArchiveObject: %v", err)
		}
		if key != "backup-node1-2026-10-08T10-00-00.tar.gz" {
			t.Fatalf("key = %q, want the newest backup-*.tar.gz overall", key)
		}
	})
}

func TestNewestArchiveObject_Errors(t *testing.T) {
	if _, err := (&Service{}).newestArchiveObject(context.Background(), "sfapp", ""); err == nil || !strings.Contains(err.Error(), "without a shared archive") {
		t.Fatalf("unconfigured store must error, got %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0"?><ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer srv.Close()
	svc := &Service{S3: backups.S3Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "ak", SecretKey: "sk"}}
	if _, err := svc.newestArchiveObject(context.Background(), "sfapp", ""); err == nil || !strings.Contains(err.Error(), "no whole-disk archive found") {
		t.Fatalf("empty store must error, got %v", err)
	}
}

// TestMoverEndpoint pins the BUG-030 follow-up: the mover runs in a container,
// where 127.0.0.1 is the container itself, so a loopback store endpoint must be
// rewritten to host.docker.internal (the published ingress is reachable there).
func TestMoverEndpoint(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8333":  "http://host.docker.internal:8333",
		"https://localhost:9000": "https://host.docker.internal:9000",
		"http://localhost/":      "http://host.docker.internal/",
		"s3.example.com:443":     "s3.example.com:443", // non-loopback untouched
	}
	for in, want := range cases {
		if got := moverEndpoint(in); got != want {
			t.Errorf("moverEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeDockerOnPath installs a fake `docker` script first on PATH and returns a restore func.
func fakeDockerOnPath(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/docker", []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestStartMoverService_HangingCreateIsTolerated pins the BUG-030 follow-up: a
// `docker service create` that never returns must not stall the move once the
// service exists.
func TestStartMoverService_HangingCreateIsTolerated(t *testing.T) {
	fakeDockerOnPath(t, "#!/bin/sh\nif [ \"$1\" = service ] && [ \"$2\" = inspect ]; then exit 0; fi\nif [ \"$1\" = service ] && [ \"$2\" = create ]; then sleep 5; exit 0; fi\nexit 0\n")
	start := time.Now()
	if err := startMoverService(context.Background(), []string{"service", "create", "--name", "pmcluster-move-x-abc", "rclone/rclone:latest"}, "pmcluster-move-x-abc"); err != nil {
		t.Fatalf("a hanging create must be tolerated once the service exists: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("startMoverService took %v, want a fast return", d)
	}
}

// TestStartMoverService_ReportsCreateFailure keeps real CLI errors (e.g. the
// exit-125 unknown-flag message) surfaced to the caller.
func TestStartMoverService_ReportsCreateFailure(t *testing.T) {
	fakeDockerOnPath(t, "#!/bin/sh\nif [ \"$1\" = service ] && [ \"$2\" = inspect ]; then exit 1; fi\necho \"unknown flag: --host-add\" >&2\nexit 125\n")
	err := startMoverService(context.Background(), []string{"service", "create", "--name", "pmcluster-move-x-abc", "rclone/rclone:latest"}, "pmcluster-move-x-abc")
	if err == nil {
		t.Fatal("a failing create must return an error")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("the error must carry the CLI output, got %v", err)
	}
}

// TestRestoreArchiveToNode_NoStore: without an object store the exported
// restore primitive fails loudly instead of creating a mover that cannot pull.
func TestRestoreArchiveToNode_NoStore(t *testing.T) {
	err := (&Service{}).RestoreArchiveToNode(context.Background(), "backup-x.tar.gz", "sfapp", "nxt-sw-2-m", false)
	if err == nil || !strings.Contains(err.Error(), "configure the object store") {
		t.Fatalf("err = %v, want a no-object-store error", err)
	}
}

// TestRestoreArchiveToNode_RoutesMoverToOwner runs the exported restore
// primitive against a fake docker CLI and asserts the one-shot service is
// pinned to the owning node, pulls the archive object from the store, and
// keeps the archive-content guard ahead of the wipe.
func TestRestoreArchiveToNode_RoutesMoverToOwner(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "argv.txt")
	fakeDockerOnPath(t, fmt.Sprintf(`#!/bin/sh
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
`, capture, capture))

	svc := &Service{
		S3:         backups.S3Config{Endpoint: "http://127.0.0.1:8333", Bucket: "pmcluster-backups", AccessKey: "ak", SecretKey: "sk"},
		VolumeRoot: "/var/stack/data",
	}
	if err := svc.RestoreArchiveToNode(context.Background(), "backup-node1-2026-10-08T10-00-00.tar.gz", "sfapp", "nxt-sw-2-m", false); err != nil {
		t.Fatalf("RestoreArchiveToNode: %v", err)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured argv: %v", err)
	}
	joined := strings.Join(strings.Fields(string(data)), " ")
	for _, want := range []string{
		"--constraint node.hostname==nxt-sw-2-m",
		"--restart-condition none",
		"--mount type=bind,source=/var/stack/data,destination=/data",
		"--host host.docker.internal:host-gateway",
		"RCLONE_CONFIG_SW_ENDPOINT=http://host.docker.internal:8333",
		"RCLONE_CONFIG_SW_ACCESS_KEY_ID=ak",
		"RCLONE_CONFIG_SW_SECRET_ACCESS_KEY=sk",
		"RCLONE_CONFIG_SW_FORCE_PATH_STYLE=true",
		"rclone copyto sw:pmcluster-backups/backup-node1-2026-10-08T10-00-00.tar.gz /tmp/m.tgz",
		"the archive does not contain backup/data/sfapp",
		"mv /tmp/x/backup/data/sfapp /data/",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("mover argv missing %q:\n%s", want, joined)
		}
	}
	// BUG-031: the archive-content guard must run BEFORE the target's subtree
	// is wiped — a wrong archive must never erase the owner's /data/sfapp.
	guardIdx := strings.Index(joined, "the archive does not contain")
	rmIdx := strings.Index(joined, "rm -rf /data/sfapp")
	if guardIdx == -1 || rmIdx == -1 || guardIdx >= rmIdx {
		t.Errorf("the archive-content guard must precede rm -rf /data/sfapp:\n%s", joined)
	}
}

// TestRestoreArchiveToNode_DefaultVolumeRoot: an unset VolumeRoot falls back to
// the manifest default so the mover binds the right host directory.
func TestRestoreArchiveToNode_DefaultVolumeRoot(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "argv.txt")
	fakeDockerOnPath(t, fmt.Sprintf(`#!/bin/sh
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
`, capture, capture))

	svc := &Service{S3: backups.S3Config{Endpoint: "http://127.0.0.1:8333", Bucket: "b", AccessKey: "ak", SecretKey: "sk"}}
	if err := svc.RestoreArchiveToNode(context.Background(), "backup-x.tar.gz", "sfapp", "nxt-sw-2-m", false); err != nil {
		t.Fatalf("RestoreArchiveToNode: %v", err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured argv: %v", err)
	}
	joined := strings.Join(strings.Fields(string(data)), " ")
	if !strings.Contains(joined, "--mount type=bind,source=/var/stack/data,destination=/data") {
		t.Errorf("unset VolumeRoot must bind the default /var/stack/data:\n%s", joined)
	}
}

// TestRestoreArchiveToNode_FromOffsite: with fromOffsite=true the mover's rclone
// remote must point at the OFFSITE backup_s3_* destination (endpoint + creds +
// bucket) instead of the in-cluster SeaweedFS store. The store (127.0.0.1:8333)
// must not appear anywhere in the mover argv.
func TestRestoreArchiveToNode_FromOffsite(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "argv.txt")
	fakeDockerOnPath(t, fmt.Sprintf(`#!/bin/sh
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
`, capture, capture))

	svc := &Service{
		S3:        backups.S3Config{Endpoint: "http://127.0.0.1:8333", Bucket: "pmcluster-backups", AccessKey: "storeak", SecretKey: "storesk"},
		OffsiteS3: backups.S3Config{Endpoint: "https://r2.example.com", Bucket: "offsite-bucket", AccessKey: "offak", SecretKey: "offsk", Region: "auto"},
	}
	if err := svc.RestoreArchiveToNode(context.Background(), "backup-node1-2026-10-08T10-00-00.tar.gz", "sfapp", "nxt-sw-2-m", true); err != nil {
		t.Fatalf("RestoreArchiveToNode: %v", err)
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
	if strings.Contains(joined, "127.0.0.1") || strings.Contains(joined, "storeak") || strings.Contains(joined, "pmcluster-backups") {
		t.Errorf("offsite restore must not use the in-cluster store:\n%s", joined)
	}
}
