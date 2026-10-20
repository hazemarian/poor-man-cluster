package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEdgeBearerValidRequiresExactManagedToken(t *testing.T) {
	const token = "managed-edge-token"
	for _, tc := range []struct {
		name   string
		header string
		want   bool
	}{
		{name: "exact", header: "Bearer " + token, want: true},
		{name: "scheme case insensitive", header: "bearer " + token, want: true},
		{name: "wrong token", header: "Bearer another-token", want: false},
		{name: "client token cannot substitute", header: "Bearer user-api-token", want: false},
		{name: "empty", header: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EdgeBearerValid(tc.header, token); got != tc.want {
				t.Fatalf("EdgeBearerValid(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestEdgeForwardUsesReservedRouteAndSanitizedEnvelope(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w := Window{Start: now.Add(-time.Hour), End: now}
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotAuth = req.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"available","observed_at":"2026-10-20T12:00:00Z","window":{"start":"2026-10-20T11:00:00Z","end":"2026-10-20T12:00:00Z"},"rows":[{"body":"ok"}],"truncated":false}`))
	}))
	t.Cleanup(server.Close)
	source, err := NewEdgeForward(EdgeForwardConfig{BaseURL: server.URL, EdgeToken: "managed-token"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := source.Logs(context.Background(), LogQuery{Window: w, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusAvailable || len(result.Rows) != 1 {
		t.Fatalf("result = %#v, want one available row", result)
	}
	if gotPath != "/internal/observability/logs" || gotAuth != "Bearer managed-token" {
		t.Fatalf("forwarded request = path %q auth %q", gotPath, gotAuth)
	}
}

func TestEdgeForwardRejectsInvalidTraceIDBeforeRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	t.Cleanup(server.Close)
	source, err := NewEdgeForward(EdgeForwardConfig{BaseURL: server.URL, EdgeToken: "managed-token"})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := NewWindow(time.Now(), time.Hour)
	if _, err := source.Trace(context.Background(), w, strings.Repeat("g", 32)); err == nil {
		t.Fatal("invalid trace ID unexpectedly accepted")
	}
	if calls != 0 {
		t.Fatalf("edge calls = %d, want zero", calls)
	}
}

func TestEdgeForwardDoesNotFollowRedirectsWithManagedToken(t *testing.T) {
	var targetCalls int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/target", http.StatusFound)
	}))
	t.Cleanup(entry.Close)

	shared := &http.Client{}
	source, err := NewEdgeForward(EdgeForwardConfig{BaseURL: entry.URL, EdgeToken: "managed-token", HTTPClient: shared})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := NewWindow(time.Now().Add(-time.Minute), time.Minute)
	if _, err := source.Logs(context.Background(), LogQuery{Window: w, Limit: 1}); err == nil {
		t.Fatal("redirect unexpectedly produced a successful result")
	}
	if targetCalls != 0 {
		t.Fatalf("redirect target calls = %d, want zero", targetCalls)
	}
	if shared.CheckRedirect != nil {
		t.Fatal("NewEdgeForward mutated the injected client")
	}
}
