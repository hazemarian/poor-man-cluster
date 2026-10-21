package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func waitParseableHealthy(t *testing.T, ctx context.Context, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.Fatalf("context done waiting for Parseable healthy: %v", ctx.Err())
		default:
		}
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				t.Logf("Parseable healthy (HTTP %d)", resp.StatusCode)
				return
			}
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("timed out waiting for Parseable at %s", url)
}

// parseableHasStream queries Parseable's stream list with the admin credential
// and reports whether the named stream exists. Returns the raw body so a
// failure shows what the server actually said.
