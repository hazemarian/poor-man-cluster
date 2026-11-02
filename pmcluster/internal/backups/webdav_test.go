package backups

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PutWebDAV is the agent's only network write, so it is tested against a real
// HTTP server: the failure that matters is a silent one (a 401 or a 507 that
// nobody checked), and that only shows up when the status is read back.
func TestPutWebDAVUploadsTheFile(t *testing.T) {
	var gotName, gotBody, gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		gotName = filepath.Base(r.URL.Path)
		gotUser, gotPass, _ = r.BasicAuth()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.ContentLength <= 0 {
			t.Errorf("ContentLength = %d, want the file size", r.ContentLength)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	dir := t.TempDir()
	p := filepath.Join(dir, "backup-node1-2026-11-02T19-00-00-stack-app.tar.gz")
	if err := os.WriteFile(p, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	userinfo := strings.Replace(srv.URL, "http://", "http://user:p%40ss@", 1) + "/buckets/pmcluster-backups"
	if err := PutWebDAV(context.Background(), userinfo, p); err != nil {
		t.Fatalf("PutWebDAV: %v", err)
	}
	if gotName != filepath.Base(p) {
		t.Errorf("server saw %q, want %q", gotName, filepath.Base(p))
	}
	if gotBody != "payload" {
		t.Errorf("body = %q", gotBody)
	}
	// Credentials embedded in the URL must reach the server decoded, and the
	// URL must not leak them into the path.
	if gotUser != "user" || gotPass != "p@ss" {
		t.Errorf("basic auth = %q/%q", gotUser, gotPass)
	}
}

func TestPutWebDAVFailsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInsufficientStorage)
		_, _ = w.Write([]byte("507 no space"))
	}))
	defer srv.Close()

	p := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := PutWebDAV(context.Background(), srv.URL, p)
	if err == nil {
		t.Fatal("PutWebDAV accepted a 507")
	}
	// The body is what tells an operator why the store refused it.
	if !strings.Contains(err.Error(), "507") || !strings.Contains(err.Error(), "no space") {
		t.Errorf("error hides the server's reason: %v", err)
	}
}

func TestPutWebDAVRejectsEmptyEndpoint(t *testing.T) {
	if err := PutWebDAV(context.Background(), "", "x"); err == nil {
		t.Fatal("want an error with no endpoint")
	}
}
