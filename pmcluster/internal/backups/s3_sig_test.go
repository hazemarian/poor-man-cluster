package backups

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestSignV4Headers_SignsAmzDate pins the BUG-029 fix: x-amz-date must be in
// SignedHeaders and part of the signed payload. SeaweedFS (the in-cluster
// store) rejects a request that omits it with 403 SignatureDoesNotMatch.
func TestSignV4Headers_SignsAmzDate(t *testing.T) {
	cfg := S3Config{Endpoint: "http://127.0.0.1:8333", Bucket: "pmcluster-backups", AccessKey: "ak", SecretKey: "sk"}
	t1 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC) // same day, one second later

	h1 := signV4HeadersQuery(cfg, "k.tar.gz", url.Values(nil), t1)
	h2 := signV4HeadersQuery(cfg, "k.tar.gz", url.Values(nil), t2)

	auth := h1.Get("Authorization")
	if !strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Fatalf("x-amz-date must be signed, got %q", auth)
	}
	if h1.Get("x-amz-date") != "20260102T030405Z" {
		t.Fatalf("x-amz-date header = %q", h1.Get("x-amz-date"))
	}
	// With the date in the signed payload the signature changes per second;
	// before the fix it was stable within a day (only the scope date changed).
	if auth == h2.Get("Authorization") {
		t.Fatal("signature did not change with x-amz-date: the date is not part of the signed payload")
	}
}
