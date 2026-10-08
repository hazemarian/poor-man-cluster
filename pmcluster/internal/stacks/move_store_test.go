package stacks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
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
	if !strings.Contains(strings.Join(args, " "), "--host-add host.docker.internal:host-gateway") {
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

// TestNewestArchiveObject picks the newest whole-disk archive and never a
// control-plane archive or a non-tarball.
func TestNewestArchiveObject(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>pmcluster-backups</Name><KeyCount>4</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>
  <Contents><Key>backup-node1-2026-10-08T09-00-00.tar.gz</Key><LastModified>2026-10-08T09:00:01.000Z</LastModified><Size>1</Size></Contents>
  <Contents><Key>backup-node1-2026-10-08T10-00-00.tar.gz</Key><LastModified>2026-10-08T10:00:01.000Z</LastModified><Size>1</Size></Contents>
  <Contents><Key>pmcluster-ctlplane-node1-2026-10-08T11-00-00.tar.gz</Key><LastModified>2026-10-08T11:00:01.000Z</LastModified><Size>1</Size></Contents>
  <Contents><Key>notes.txt</Key><LastModified>2026-10-08T12:00:01.000Z</LastModified><Size>1</Size></Contents>
</ListBucketResult>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("prefix"); got != "backup-" {
			t.Errorf("prefix = %q, want backup-", got)
		}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	svc := &Service{S3: backups.S3Config{Endpoint: srv.URL, Bucket: "pmcluster-backups", AccessKey: "ak", SecretKey: "sk"}}
	key, err := svc.newestArchiveObject(context.Background(), "sfapp")
	if err != nil {
		t.Fatalf("newestArchiveObject: %v", err)
	}
	if key != "backup-node1-2026-10-08T10-00-00.tar.gz" {
		t.Fatalf("key = %q, want the newest backup-*.tar.gz (ctlplane + non-tarball excluded)", key)
	}
}

func TestNewestArchiveObject_Errors(t *testing.T) {
	if _, err := (&Service{}).newestArchiveObject(context.Background(), "sfapp"); err == nil || !strings.Contains(err.Error(), "without a shared archive") {
		t.Fatalf("unconfigured store must error, got %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0"?><ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer srv.Close()
	svc := &Service{S3: backups.S3Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "ak", SecretKey: "sk"}}
	if _, err := svc.newestArchiveObject(context.Background(), "sfapp"); err == nil || !strings.Contains(err.Error(), "no whole-disk archive found") {
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
