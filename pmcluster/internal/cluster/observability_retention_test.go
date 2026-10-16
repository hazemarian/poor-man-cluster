package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestConvergeObservabilityRetention_PayloadShape pins the contract that was
// established by probing the running Parseable: the body is a SEQUENCE of task
// objects and `action` is mandatory. A bare object is rejected by the server
// with "invalid type: map, expected a sequence", and an object without `action`
// with "missing field `action`" — both were observed live, so the shape here is
// what the server actually accepts rather than what the docs imply.
func TestConvergeObservabilityRetention_PayloadShape(t *testing.T) {
	type captured struct {
		stream string
		auth   string
		tasks  []retentionTask
	}
	var got []captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		var tasks []retentionTask
		if err := json.Unmarshal(body, &tasks); err != nil {
			t.Errorf("body is not a JSON sequence of tasks: %v (body=%s)", err, body)
		}
		u, p, ok := r.BasicAuth()
		if !ok {
			t.Error("request is missing basic auth")
		}
		stream := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/logstream/"), "/retention")
		got = append(got, captured{stream: stream, auth: u + ":" + p, tasks: tasks})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"duration":"30d","action":"delete","description":"ok"}]`))
	}))
	defer srv.Close()

	if err := ConvergeObservabilityRetention(context.Background(), srv.URL, "admin@x.com", "pw", 30, 14, 10); err != nil {
		t.Fatalf("ConvergeObservabilityRetention: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 streams to be converged, got %d: %+v", len(got), got)
	}
	want := map[string]string{
		"pmcluster-logs":    "30d",
		"pmcluster-metrics": "14d",
		"pmcluster-traces":  "10d",
	}
	for _, c := range got {
		if d, ok := want[c.stream]; !ok {
			t.Errorf("unexpected stream %q", c.stream)
		} else if len(c.tasks) != 1 || c.tasks[0].Duration != d {
			t.Errorf("stream %s: tasks=%+v, want one task with duration %s", c.stream, c.tasks, d)
		}
		if len(c.tasks) == 1 && c.tasks[0].Action == "" {
			t.Errorf("stream %s: action is mandatory for Parseable, got empty", c.stream)
		}
		if c.auth != "admin@x.com:pw" {
			t.Errorf("stream %s: basic auth = %q, want the managed credential", c.stream, c.auth)
		}
	}
}

// TestConvergeObservabilityRetention_SkipsUnset verifies an unconfigured
// retention (zero or negative) leaves that stream alone instead of clearing
// whatever is already set there.
func TestConvergeObservabilityRetention_SkipsUnset(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := ConvergeObservabilityRetention(context.Background(), srv.URL, "u", "p", 0, -1, 7); err != nil {
		t.Fatalf("ConvergeObservabilityRetention: %v", err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0], "pmcluster-traces") {
		t.Errorf("expected only pmcluster-traces to be set, got %v", hits)
	}
}

// TestConvergeObservabilityRetention_ServerErrorSurfaces verifies a rejection
// carries the server's own message, so the daemon can warn with the reason.
func TestConvergeObservabilityRetention_ServerErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid type: map, expected a sequence"))
	}))
	defer srv.Close()

	err := ConvergeObservabilityRetention(context.Background(), srv.URL, "u", "p", 30, 0, 0)
	if err == nil {
		t.Fatal("expected an error when the server rejects the payload")
	}
	if !strings.Contains(err.Error(), "expected a sequence") {
		t.Errorf("error should carry the server's reason, got: %v", err)
	}
}
