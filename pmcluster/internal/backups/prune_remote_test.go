package backups

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The store and the bucket were never pruned: an upload loop with no delete grows
// until the bucket is full and every upload fails. These tests pin the policy that
// now governs all three legs to the same rules as the node-local archive.
func TestPruneS3DeletesOldObjectsAndKeepsRecentOnes(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	fresh := ArtifactName("shop", "node1", now.Add(-1*time.Hour))
	old := ArtifactName("shop", "node1", now.Add(-40*24*time.Hour))

	var mu sync.Mutex
	deleted := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			fmt.Fprintf(w, `<?xml version="1.0"?><ListBucketResult>
				<Contents><Key>%s</Key><LastModified>%s</LastModified><Size>1</Size></Contents>
				<Contents><Key>%s</Key><LastModified>%s</LastModified><Size>1</Size></Contents>
				<IsTruncated>false</IsTruncated></ListBucketResult>`,
				fresh, now.Add(-time.Hour).Format(time.RFC3339), old, now.Add(-960*time.Hour).Format(time.RFC3339))
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted[strings.TrimPrefix(r.URL.Path, "/bkt/")] = true
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := S3Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "a", SecretKey: "b", Region: "auto"}
	// KeepLast is the safety floor (never delete the newest N of a stack); this test
	// is about the age rule, so it is lowered to one artifact.
	policy := DefaultRetention()
	policy.KeepLast = 1
	res, err := PruneS3(context.Background(), cfg, policy, now, false)
	if err != nil {
		t.Fatalf("PruneS3: %v", err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != old {
		t.Fatalf("deleted %v, want only %s", res.Deleted, old)
	}
	if !deleted[old] {
		t.Fatalf("the old object was reported deleted but no DELETE was sent")
	}
	if deleted[fresh] {
		t.Fatalf("the newest artifact was deleted: retention must never do that")
	}
}

// A dry run must not delete anything — doing the work and calling it hypothetical
// is how "dry run" destroys data.
func TestPruneS3DryRunSendsNoDelete(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	old := ArtifactName("shop", "node1", now.Add(-40*24*time.Hour))
	fresh := ArtifactName("shop", "node1", now.Add(-time.Hour))
	var dels int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			dels++
			return
		}
		fmt.Fprintf(w, `<?xml version="1.0"?><ListBucketResult>
			<Contents><Key>%s</Key><LastModified>%s</LastModified><Size>1</Size></Contents>
			<Contents><Key>%s</Key><LastModified>%s</LastModified><Size>1</Size></Contents>
			<IsTruncated>false</IsTruncated></ListBucketResult>`,
			fresh, now.Format(time.RFC3339), old, now.Format(time.RFC3339))
	}))
	defer srv.Close()

	cfg := S3Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "a", SecretKey: "b", Region: "auto"}
	policy := DefaultRetention()
	policy.KeepLast = 1
	res, err := PruneS3(context.Background(), cfg, policy, now, true)
	if err != nil {
		t.Fatalf("PruneS3 dry run: %v", err)
	}
	if dels != 0 {
		t.Fatalf("dry run sent %d DELETEs", dels)
	}
	if len(res.Deleted) != 1 || !strings.Contains(res.Deleted[0], "would delete") {
		t.Fatalf("dry run did not report what it would delete: %v", res.Deleted)
	}
}

// The in-cluster store speaks WebDAV; listing is PROPFIND and removing is DELETE.
