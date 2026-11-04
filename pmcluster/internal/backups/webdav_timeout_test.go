package backups

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A store that accepts the connection and then says nothing must fail the ship
// quickly. It once held a production run for 22 minutes of wall clock and 1.6 s
// of CPU: the agent sat in epoll, the flock stayed held, and the next hourly run
// exited as "already running" — a node that looked like it was backing up.
func TestPutWebDAVFailsFastOnAStoreThatStopsAnswering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second) // never answers within the deadline
	}))
	defer srv.Close()

	old := putTimeout
	putTimeout = 200 * time.Millisecond
	defer func() { putTimeout = old }()

	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.tar.gz")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err := PutWebDAV(context.Background(), srv.URL+"/buckets/b", path)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when the store never answers")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("took %s to give up; the deadline did not bound the request", elapsed)
	}
}
