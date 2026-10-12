package backups

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The offen agent writes each archive to BOTH the local archive dir (mirrored
// into the in-cluster store over WebDAV) and the configured offsite bucket over
// S3, local copy first. So an archive sitting on disk is not evidence that the
// offsite copy landed — which is how a deleted bucket produced hours of runs
// recorded as succeeded while every offsite upload failed. These tests pin the
// three outcomes discovery must distinguish.

// s3Responder answers any ListObjectsV2 request with a fixed status + body,
// regardless of path, prefix or pagination: the request SHAPE is not the
// subject here, the recorded outcome is.
func s3Responder(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

// withS3Server points the package's client at srv for the duration of the test.
func withS3Server(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := s3HTTPClient
	s3HTTPClient = srv.Client()
	t.Cleanup(func() { s3HTTPClient = old })
}

func listBodyWith(bucket, key string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>%s</Name><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>
  <Contents><Key>%s</Key><LastModified>2026-10-12T21:00:00.000Z</LastModified></Contents>
</ListBucketResult>`, bucket, key)
}

func listBodyEmpty(bucket string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>%s</Name><KeyCount>0</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>
</ListBucketResult>`, bucket)
}

func noSuchBucket(bucket string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchBucket</Code><Message></Message><BucketName>%s</BucketName></Error>`, bucket)
}

func offsiteCfg(endpoint, bucket string) S3Config {
	return S3Config{Endpoint: endpoint, Bucket: bucket, AccessKey: "AKIA", SecretKey: "secret", Region: "eu-central-3"}
}

// TestCheckS3TargetMissingBucketIsTyped: a 404 NoSuchBucket is a configuration
// error the operator must fix, and must be recognisable as such.
func TestCheckS3TargetMissingBucketIsTyped(t *testing.T) {
	srv := s3Responder(t, http.StatusNotFound, noSuchBucket("gone-bucket"))
	defer srv.Close()
	withS3Server(t, srv)

	err := CheckS3Target(context.Background(), offsiteCfg(srv.URL, "gone-bucket"))
	if !errors.Is(err, ErrS3BucketMissing) {
		t.Fatalf("CheckS3Target error = %v, want it to wrap ErrS3BucketMissing", err)
	}
	if !strings.Contains(err.Error(), "gone-bucket") {
		t.Errorf("error does not name the bucket: %v", err)
	}
}

// TestCheckS3TargetUnreachableIsNotAMissingBucket: an endpoint that answers
// something else is an outage, not a misconfiguration. The caller treats the
// two differently — this pins that they stay distinguishable.
func TestCheckS3TargetUnreachableIsNotAMissingBucket(t *testing.T) {
	srv := s3Responder(t, http.StatusInternalServerError, "<Error><Code>InternalError</Code></Error>")
	defer srv.Close()
	withS3Server(t, srv)

	err := CheckS3Target(context.Background(), offsiteCfg(srv.URL, "some-bucket"))
	if err == nil {
		t.Fatal("CheckS3Target returned nil for a 500 response")
	}
	if errors.Is(err, ErrS3BucketMissing) {
		t.Errorf("a 500 was classified as a missing bucket: %v", err)
	}
}

// TestCheckS3TargetUnconfiguredIsNoop: no offsite configured is never an error,
// so a cluster without an offsite still applies settings and boots.
func TestCheckS3TargetUnconfiguredIsNoop(t *testing.T) {
	if err := CheckS3Target(context.Background(), S3Config{}); err != nil {
		t.Fatalf("CheckS3Target on an unconfigured target = %v, want nil", err)
	}
}

// TestCheckS3TargetEmptyBucketIsValid: a reachable, empty bucket is a valid
// destination — an offsite that has not received its first archive yet.
func TestCheckS3TargetEmptyBucketIsValid(t *testing.T) {
	srv := s3Responder(t, http.StatusOK, listBodyEmpty("fresh-bucket"))
	defer srv.Close()
	withS3Server(t, srv)

	if err := CheckS3Target(context.Background(), offsiteCfg(srv.URL, "fresh-bucket")); err != nil {
		t.Fatalf("CheckS3Target on an empty but reachable bucket = %v, want nil", err)
	}
}

// discoveryRowFor returns the recorded run whose archive path ends in name.
func discoveryRowFor(t *testing.T, rows []Run, name string) Run {
	t.Helper()
	for _, r := range rows {
		for _, p := range r.ArchivePaths {
			if strings.HasSuffix(p, name) {
				return r
			}
		}
	}
	t.Fatalf("no recorded run for %s in %d rows", name, len(rows))
	return Run{}
}

func writeDiscoveredArchive(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("not-a-real-tarball"), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
}

// TestDiscoverRecordsOffsiteMissingRatherThanSucceeded is the regression that
// matters: the local archive exists, the configured offsite bucket is reachable,
// and the offsite copy is NOT there. The run must not be recorded as a success.
func TestDiscoverRecordsOffsiteMissingRatherThanSucceeded(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dir := t.TempDir()
	const name = "backup-node1-2026-10-12T21-00-00.tar.gz"
	writeDiscoveredArchive(t, dir, name)

	srv := s3Responder(t, http.StatusOK, listBodyEmpty("pmclusterbackup"))
	defer srv.Close()
	withS3Server(t, srv)

	svc := &Local{Store: st, ArchiveDir: dir, Offsite: offsiteCfg(srv.URL, "pmclusterbackup")}
	rows, err := svc.List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	row := discoveryRowFor(t, rows, name)
	if row.Status == StatusSucceeded {
		t.Fatalf("run recorded as %q although its offsite copy never landed", row.Status)
	}
	if row.Status != StatusOffsiteMissing {
		t.Errorf("status = %q, want %q", row.Status, StatusOffsiteMissing)
	}
	if !strings.Contains(row.ErrorMessage, "pmclusterbackup") {
		t.Errorf("note does not name the bucket: %q", row.ErrorMessage)
	}
}

// TestDiscoverRecordsSucceededWhenTheOffsiteCopyLanded: the same setup with the
// object present is a success.
func TestDiscoverRecordsSucceededWhenTheOffsiteCopyLanded(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dir := t.TempDir()
	const name = "backup-node1-2026-10-12T21-00-00.tar.gz"
	writeDiscoveredArchive(t, dir, name)

	srv := s3Responder(t, http.StatusOK, listBodyWith("pmclusterbackup", name))
	defer srv.Close()
	withS3Server(t, srv)

	svc := &Local{Store: st, ArchiveDir: dir, Offsite: offsiteCfg(srv.URL, "pmclusterbackup")}
	rows, err := svc.List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := discoveryRowFor(t, rows, name).Status; got != StatusSucceeded {
		t.Errorf("status = %q, want %q", got, StatusSucceeded)
	}
}

// TestDiscoverRecordsUnverifiedWhenTheBucketIsGone: with the bucket deleted, the
// offsite leg cannot be verified at all — which is still not a success, and the
// note says which of the two situations it was.
func TestDiscoverRecordsUnverifiedWhenTheBucketIsGone(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dir := t.TempDir()
	const name = "backup-node1-2026-10-12T21-00-00.tar.gz"
	writeDiscoveredArchive(t, dir, name)

	srv := s3Responder(t, http.StatusNotFound, noSuchBucket("nextrum-bu"))
	defer srv.Close()
	withS3Server(t, srv)

	svc := &Local{Store: st, ArchiveDir: dir, Offsite: offsiteCfg(srv.URL, "nextrum-bu")}
	rows, err := svc.List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	row := discoveryRowFor(t, rows, name)
	if row.Status != StatusOffsiteMissing {
		t.Errorf("status = %q, want %q", row.Status, StatusOffsiteMissing)
	}
	if !strings.Contains(row.ErrorMessage, "unverified") {
		t.Errorf("note should say the offsite copy could not be verified: %q", row.ErrorMessage)
	}
}

// TestDiscoverWithoutOffsiteKeepsTheOldBehaviour: a cluster with no offsite
// configured records discovered runs as succeeded, exactly as before.
func TestDiscoverWithoutOffsiteKeepsTheOldBehaviour(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dir := t.TempDir()
	const name = "backup-node1-2026-10-12T21-00-00.tar.gz"
	writeDiscoveredArchive(t, dir, name)

	svc := &Local{Store: st, ArchiveDir: dir}
	rows, err := svc.List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := discoveryRowFor(t, rows, name).Status; got != StatusSucceeded {
		t.Errorf("status = %q, want %q", got, StatusSucceeded)
	}
}

// TestRestore_AcceptsARunWhoseOffsiteCopyIsMissing: only the offsite COPY is
// absent; the archive on disk is the same snapshot, and restore is local-first.
// Refusing here would block a restore during exactly the incident — an offsite
// outage — where an operator may need one.
func TestRestore_AcceptsARunWhoseOffsiteCopyIsMissing(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	archiveDir := t.TempDir()
	destRoot := t.TempDir()

	const name = "backup-node1-2026-10-12T21-00-00.tar.gz"
	writeTarGzDirs(t, archiveDir, name, "backup/data", map[string]string{
		"e2erestore/app_data/marker.txt": "original-data-v1\n",
	})

	// Record the run the way discovery now does when the offsite copy is gone.
	if err := st.RecordDiscoveredBackupStatus(ctx, name, filepath.Join(archiveDir, name), 0,
		StatusOffsiteMissing, "offsite copy missing"); err != nil {
		t.Fatalf("RecordDiscoveredBackupStatus: %v", err)
	}
	rows, err := st.ListBackups(ctx, 1)
	if err != nil || len(rows) == 0 {
		t.Fatalf("ListBackups: %v (rows=%d)", err, len(rows))
	}
	id := rows[0].ID

	svc := &Local{Store: st, ArchiveDir: archiveDir}
	n, err := svc.Restore(ctx, id, destRoot, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore refused a run whose archive is on disk: %v", err)
	}
	if n == 0 {
		t.Fatal("Restore restored 0 files")
	}
	got, err := os.ReadFile(filepath.Join(destRoot, "e2erestore", "app_data", "marker.txt"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != "original-data-v1\n" {
		t.Errorf("restored content = %q, want the original bytes", string(got))
	}
}
