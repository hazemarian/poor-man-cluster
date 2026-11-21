package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpenShellSession_Contract pins the wire contract of the CLI shell against the daemon's real
// routes: POST /api/stacks/{name}/shell returns a service_id, and the token travels as a bearer header.
// The daemon side cannot be exercised without a swarm, but a wrong path or a missing header is the kind
// of defect that only shows up in an operator's terminal otherwise.
func TestOpenShellSession_Contract(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service_id":"svc123","max_seconds":1800,"node":"nxt-sw-5-m"}`))
	}))
	defer srv.Close()

	sess, err := openShellSession(context.Background(), srv.URL, "tok", "aarma", "")
	if err != nil {
		t.Fatalf("openShellSession: %v", err)
	}
	if gotPath != "/api/stacks/aarma/shell" {
		t.Errorf("path = %q, want /api/stacks/aarma/shell", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want a bearer token", gotAuth)
	}
	if sess.ServiceID != "svc123" || sess.Node != "nxt-sw-5-m" || sess.MaxSeconds != 1800 {
		t.Errorf("session = %+v, want the daemon's answer decoded", sess)
	}
}

// TestOpenShellSession_ReportsTheDaemonRefusal — a refused session must say what the daemon said. An
// operator who gets "exit status 1" learns nothing; a body with a reason is actionable.
func TestOpenShellSession_ReportsTheDaemonRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "shell sessions are not available on this node", http.StatusNotImplemented)
	}))
	defer srv.Close()

	_, err := openShellSession(context.Background(), srv.URL, "tok", "aarma", "")
	if err == nil {
		t.Fatal("a 501 was reported as success")
	}
	if !strings.Contains(err.Error(), "not available on this node") {
		t.Errorf("the daemon's reason was lost: %v", err)
	}
}

// TestCloseShellSession_UsesTheDeleteRoute — the session must be removed by the id the daemon gave,
// because the daemon removes the container when the socket closes and this is the belt to that braces.
func TestCloseShellSession_UsesTheDeleteRoute(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := closeShellSession(context.Background(), srv.URL, "tok", "svc123"); err != nil {
		t.Fatalf("closeShellSession: %v", err)
	}
	if gotPath != "/api/shells/svc123" || gotMethod != http.MethodDelete {
		t.Errorf("got %s %s, want DELETE /api/shells/svc123", gotMethod, gotPath)
	}
}
