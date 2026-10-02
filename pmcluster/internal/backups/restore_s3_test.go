package backups

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
)

func TestVolumeMatch(t *testing.T) {
	cases := []struct {
		rel, volume string
		want        bool
	}{
		{"demo/db_data/postgres", "db_data", true},
		{"demo/db_data", "db_data", true},
		{"db_data", "db_data", true},
		{"demo/other/data", "db_data", false},
		{"demo/db_data_extra/file", "db_data", false}, // prefix only, not a segment
		{"other/db_data/file", "db_data", true},       // any stack
		{"", "db_data", false},
		{"demo/db_data/file", "", true}, // empty volume = everything
	}
	for _, c := range cases {
		if got := volumeMatch(c.rel, c.volume); got != c.want {
			t.Errorf("volumeMatch(%q, %q) = %v, want %v", c.rel, c.volume, got, c.want)
		}
	}
}

func TestRestore_VolumeFilter(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()
	archive := filepath.Join(dir, "run.tar.gz")
	writeTarGzDirs(t, dir, "run.tar.gz", "backup/data", map[string]string{
		"demo/db_data/postgres/PG_VERSION": "16",
		"demo/other/file":                  "skip me",
	})
	svc := &Local{Store: st}
	id, _ := st.CreateBackup(context.Background(), "demo", 1)
	if err := st.FinishBackup(context.Background(), id, "succeeded", archive, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	destRoot := t.TempDir()
	n, err := svc.Restore(context.Background(), id, destRoot, RestoreOptions{Volume: "db_data"})
	if err != nil {
		t.Fatalf("Restore(volume=db_data): %v", err)
	}
	if n != 1 {
		t.Errorf("restored files = %d, want 1 (only the db_data entry)", n)
	}
	if _, err := os.Stat(filepath.Join(destRoot, "demo", "db_data", "postgres", "PG_VERSION")); err != nil {
		t.Errorf("db_data entry missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destRoot, "demo", "other", "file")); err == nil {
		t.Error("non-matching volume entry was restored — filter leaked")
	}
}

func TestRestore_S3FallbackFetches(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()
	archive := filepath.Join(dir, "run.tar.gz")
	writeTarGzDirs(t, dir, "run.tar.gz", "backup/data", map[string]string{
		"demo/db_data/postgres/PG_VERSION": "16",
	})
	archiveBytes, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("ReadFile archive: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.Header.Get("Authorization") == "" || !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Errorf("missing SigV4 Authorization header: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("x-amz-content-sha256") != emptyPayloadSHA {
			t.Errorf("x-amz-content-sha256 = %q, want empty payload hash", r.Header.Get("x-amz-content-sha256"))
		}
		if !strings.HasSuffix(r.URL.Path, "/pmcluster-backup-test-bucket/run.tar.gz") {
			t.Errorf("path = %q, want bucket + object key", r.URL.Path)
		}
		_, _ = w.Write(archiveBytes)
	}))
	defer srv.Close()

	old := s3HTTPClient
	s3HTTPClient = srv.Client()
	defer func() { s3HTTPClient = old }()

	svc := &Local{Store: st, S3: S3Config{
		Endpoint:  srv.URL,
		Bucket:    "pmcluster-backup-test-bucket",
		AccessKey: "test-access",
		SecretKey: "test-secret",
		Region:    "auto",
	}}
	id, _ := st.CreateBackup(context.Background(), "demo", 1)
	if err := st.FinishBackup(context.Background(), id, "succeeded", archive, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}
	// The local archive is gone: the restore must fetch it from S3.
	if err := os.Remove(archive); err != nil {
		t.Fatalf("Remove local archive: %v", err)
	}

	destRoot := t.TempDir()
	n, err := svc.Restore(context.Background(), id, destRoot, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore (S3 fallback): %v", err)
	}
	if n != 1 {
		t.Errorf("restored files = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(destRoot, "demo", "db_data", "postgres", "PG_VERSION")); err != nil {
		t.Errorf("restored file missing: %v", err)
	}
}

func TestRestore_NoS3AndMissingArchiveErrors(t *testing.T) {
	st := newTestStore(t)
	archive := filepath.Join(t.TempDir(), "gone.tar.gz")
	svc := &Local{Store: st} // no S3 configured
	id, _ := st.CreateBackup(context.Background(), "demo", 1)
	if err := st.FinishBackup(context.Background(), id, "succeeded", archive, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}
	_, err := svc.Restore(context.Background(), id, t.TempDir(), RestoreOptions{})
	if err == nil {
		t.Fatal("Restore succeeded with a missing local archive and no S3")
	}
	if !strings.Contains(err.Error(), "not found locally and no offsite S3 is configured") {
		t.Errorf("error should explain the missing archive + S3, got: %v", err)
	}
}

func TestRestore_FromS3WithoutConfigErrors(t *testing.T) {
	st := newTestStore(t)
	archive := filepath.Join(t.TempDir(), "run.tar.gz")
	writeTarGzDirs(t, t.TempDir(), "run.tar.gz", "backup/data", map[string]string{
		"demo/db_data/postgres/PG_VERSION": "16",
	})
	svc := &Local{Store: st} // no S3 configured
	id, _ := st.CreateBackup(context.Background(), "demo", 1)
	if err := st.FinishBackup(context.Background(), id, "succeeded", archive, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}
	_, err := svc.Restore(context.Background(), id, t.TempDir(), RestoreOptions{FromS3: true})
	if err == nil {
		t.Fatal("Restore(FromS3) succeeded with no S3 configured")
	}
	if !strings.Contains(err.Error(), "no S3 is configured") {
		t.Errorf("error should explain S3 is unconfigured, got: %v", err)
	}
}

func TestSignV4Headers(t *testing.T) {
	now, err := time.Parse(time.RFC3339, "2026-09-30T00:00:00Z")
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	h := signV4Headers(S3Config{
		Endpoint:  "https://acct.r2.cloudflarestorage.com",
		Bucket:    "my-bucket",
		AccessKey: "AKIDEXAMPLE",
		SecretKey: "secret",
		Region:    "auto",
	}, "backup-node-2026-09-30T00-00-00.tar.gz", now)

	auth := h.Get("Authorization")
	for _, want := range []string{
		"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260930/auto/s3/aws4_request",
		"SignedHeaders=host;x-amz-content-sha256",
		"Signature=",
	} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization %q missing %q", auth, want)
		}
	}
	if h.Get("x-amz-date") != "20260930T000000Z" {
		t.Errorf("x-amz-date = %q", h.Get("x-amz-date"))
	}
	if h.Get("x-amz-content-sha256") != emptyPayloadSHA {
		t.Errorf("x-amz-content-sha256 = %q", h.Get("x-amz-content-sha256"))
	}
}

func TestFetchS3Object_WritesFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "archive-bytes")
	}))
	defer srv.Close()

	old := s3HTTPClient
	s3HTTPClient = srv.Client()
	defer func() { s3HTTPClient = old }()

	dst := filepath.Join(t.TempDir(), "out.tar.gz")
	err := fetchS3Object(context.Background(), S3Config{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
	}, "obj-key", dst)
	if err != nil {
		t.Fatalf("fetchS3Object: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "archive-bytes" {
		t.Fatalf("downloaded file = %q, %v", b, err)
	}
}
