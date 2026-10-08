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
)

// s3ListFixture returns a recording httptest.Server that answers S3
// ListObjectsV2 (`?list-type=2&prefix=`) with the given XML body, asserting
// the expected SigV4 + query shape.
func s3ListFixture(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Query().Get("list-type") != "2" {
			t.Errorf("list-type = %q, want 2", r.URL.Query().Get("list-type"))
		}
		if r.URL.Query().Get("prefix") != "backup-" {
			t.Errorf("prefix = %q, want backup-", r.URL.Query().Get("prefix"))
		}
		if r.Header.Get("Authorization") == "" || !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Errorf("missing SigV4 Authorization header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, body)
	}))
}

// TestDiscoverStoreSurfacesStoreOnlyArchives proves `backup list` surfaces an
// archive that exists ONLY in the in-cluster store (produced by the agent on a
// storage node, never passed through Trigger on the leader).
func TestDiscoverStoreSurfacesStoreOnlyArchives(t *testing.T) {
	st := newTestStore(t)
	srv := s3ListFixture(t, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>pmcluster-backups</Name>
  <Prefix>backup-</Prefix>
  <KeyCount>1</KeyCount>
  <MaxKeys>1000</MaxKeys>
  <IsTruncated>false</IsTruncated>
  <Contents>
    <Key>backup-node1-2026-10-07T03-00-00.tar.gz</Key>
    <LastModified>2026-10-07T03:00:00.000Z</LastModified>
    <ETag>"abc"</ETag>
    <Size>1024</Size>
    <StorageClass>STANDARD</StorageClass>
  </Contents>
</ListBucketResult>`)
	defer srv.Close()

	old := s3HTTPClient
	s3HTTPClient = srv.Client()
	defer func() { s3HTTPClient = old }()

	svc := &Local{Store: st, S3: S3Config{
		Endpoint:  srv.URL,
		Bucket:    "pmcluster-backups",
		AccessKey: "test-access",
		SecretKey: "test-secret",
		Region:    "auto",
	}}

	// No local archive dir — the row must come entirely from the store.
	rows, err := svc.List(context.Background(), 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List returned %d rows, want 1 store-only archive", len(rows))
	}
	r := rows[0]
	if r.Status != StatusSucceeded {
		t.Errorf("row status = %q, want succeeded", r.Status)
	}
	if len(r.ArchivePaths) != 1 || r.ArchivePaths[0] != "backup-node1-2026-10-07T03-00-00.tar.gz" {
		t.Errorf("ArchivePaths = %v, want the bare object key", r.ArchivePaths)
	}
	if r.StartedAt == 0 || r.FinishedAt == 0 {
		t.Errorf("row missing timestamps: started=%d finished=%d", r.StartedAt, r.FinishedAt)
	}

	// Idempotent: a second List must not duplicate the row.
	if _, err := svc.List(context.Background(), 50); err != nil {
		t.Fatalf("second List: %v", err)
	}
	all, err := st.ListBackups(context.Background(), 50)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("after two Lists got %d rows, want 1 (idempotent)", len(all))
	}
}

// TestDiscoverStoreSkipsControlPlaneAndNonTarballs proves store discovery
// indexes only volume archives (a control-plane object and a stray non-tar.gz
// object are ignored).
func TestDiscoverStoreSkipsControlPlaneAndNonTarballs(t *testing.T) {
	st := newTestStore(t)
	srv := s3ListFixture(t, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <IsTruncated>false</IsTruncated>
  <Contents>
    <Key>backup-node1-2026-10-07T03-00-00.tar.gz</Key>
    <LastModified>2026-10-07T03:00:00Z</LastModified>
  </Contents>
  <Contents>
    <Key>backup-node1-2026-10-07T03-00-00.txt</Key>
    <LastModified>2026-10-07T03:00:00Z</LastModified>
  </Contents>
</ListBucketResult>`)
	defer srv.Close()

	old := s3HTTPClient
	s3HTTPClient = srv.Client()
	defer func() { s3HTTPClient = old }()

	svc := &Local{Store: st, S3: S3Config{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
	}}
	rows, err := svc.List(context.Background(), 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List returned %d rows, want 1 (only the .tar.gz)", len(rows))
	}
}

// TestRestoreStoreOnlyRowFetchesFromS3 proves a row whose archive path is the
// bare object key restores through fetchS3Object (no local file, no full path).
func TestRestoreStoreOnlyRowFetchesFromS3(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()
	writeTarGzDirs(t, dir, "backup-node1-2026-10-07T03-00-00.tar.gz", "backup/data", map[string]string{
		"demo/db_data/postgres/PG_VERSION": "16",
	})
	archiveBytes, err := os.ReadFile(filepath.Join(dir, "backup-node1-2026-10-07T03-00-00.tar.gz"))
	if err != nil {
		t.Fatalf("ReadFile archive: %v", err)
	}

	key := "backup-node1-2026-10-07T03-00-00.tar.gz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/b/"+key) {
			t.Errorf("path = %q, want /b/%s", r.URL.Path, key)
		}
		if r.Header.Get("Authorization") == "" || !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Errorf("missing SigV4 Authorization header")
		}
		_, _ = w.Write(archiveBytes)
	}))
	defer srv.Close()

	old := s3HTTPClient
	s3HTTPClient = srv.Client()
	defer func() { s3HTTPClient = old }()

	// Record the row exactly as discoverStore would: bare object key as the
	// archive path. No local file exists at that (relative) path.
	ctx := context.Background()
	if err := st.RecordDiscoveredBackup(ctx, key, key, 1760000000); err != nil {
		t.Fatalf("RecordDiscoveredBackup: %v", err)
	}

	svc := &Local{Store: st, S3: S3Config{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
	}}

	destRoot := t.TempDir()
	n, err := svc.Restore(ctx, 1, destRoot, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore (store-only row): %v", err)
	}
	if n != 1 {
		t.Errorf("restored files = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(destRoot, "demo", "db_data", "postgres", "PG_VERSION")); err != nil {
		t.Errorf("restored file missing (store fetch failed): %v", err)
	}
}

// TestListS3ObjectsFollowsPagination verifies continuation-token handling.
func TestListS3ObjectsFollowsPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("continuation-token") == "" {
			fmt.Fprint(w, `<?xml version="1.0"?><ListBucketResult>
				<IsTruncated>true</IsTruncated>
				<NextContinuationToken>tok-1</NextContinuationToken>
				<Contents><Key>a.tar.gz</Key><LastModified>2026-10-07T03:00:00Z</LastModified></Contents>
			</ListBucketResult>`)
		} else {
			fmt.Fprint(w, `<?xml version="1.0"?><ListBucketResult>
				<IsTruncated>false</IsTruncated>
				<Contents><Key>b.tar.gz</Key><LastModified>2026-10-07T04:00:00Z</LastModified></Contents>
			</ListBucketResult>`)
		}
	}))
	defer srv.Close()

	old := s3HTTPClient
	s3HTTPClient = srv.Client()
	defer func() { s3HTTPClient = old }()

	objects, err := listS3Objects(context.Background(), S3Config{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
	}, "backup-")
	if err != nil {
		t.Fatalf("listS3Objects: %v", err)
	}
	if len(objects) != 2 || objects[0].Key != "a.tar.gz" || objects[1].Key != "b.tar.gz" {
		t.Fatalf("objects = %+v, want [a.tar.gz b.tar.gz]", objects)
	}
}
