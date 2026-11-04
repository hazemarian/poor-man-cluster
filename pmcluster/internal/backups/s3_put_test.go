package backups

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An upload must sign the SHA-256 of the bytes it sends. The GET paths signed an
// empty payload because they carried no body; reusing that for a PUT makes every
// upload fail with 403 XAmzContentSHA256Mismatch on a strict endpoint.
func TestPutS3ObjectSignsThePayloadAndUploadsTheBytes(t *testing.T) {
	payload := []byte("stack artifact bytes")
	sum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(sum[:])

	var gotMethod, gotPath, gotAuth, gotSHA string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotSHA = r.Header.Get("x-amz-content-sha256")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	src := filepath.Join(dir, "backup-n1-2026-11-04T00-00-00-stack-web.tar.gz")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := S3Config{Endpoint: srv.URL, Bucket: "pmclusterbackup", AccessKey: "AK", SecretKey: "SK", Region: "eu-central-3"}
	if err := PutS3Object(context.Background(), cfg, "backup-n1-stack-web.tar.gz", src); err != nil {
		t.Fatalf("PutS3Object: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Fatalf("method %s, want PUT", gotMethod)
	}
	if gotPath != "/pmclusterbackup/backup-n1-stack-web.tar.gz" {
		t.Fatalf("path %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AK/") {
		t.Fatalf("authorization %q", gotAuth)
	}
	if gotSHA != wantSHA {
		t.Fatalf("content-sha256 %q, want the payload hash %q", gotSHA, wantSHA)
	}
	if string(gotBody) != string(payload) {
		t.Fatalf("body %q", gotBody)
	}
}

func TestPutS3ObjectRefusesAnIncompleteTarget(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := PutS3Object(context.Background(), S3Config{Endpoint: "http://x", Bucket: "b"}, "a.tar.gz", src)
	if err == nil {
		t.Fatal("expected an error with no keys configured")
	}
}
