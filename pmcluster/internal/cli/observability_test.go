package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

func TestObservabilityLogsJSONIsPureAndUsesRemoteBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer cli-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if req.URL.Path != "/api/observability/logs" || req.URL.Query().Get("service") != "demo_web" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"available","observed_at":"2026-10-20T12:00:00Z","window":{"start":"2026-10-20T11:00:00Z","end":"2026-10-20T12:00:00Z"},"rows":[{"p_timestamp":"2026-10-20T11:59:00Z","severity_text":"error","service.name":"demo_web","body":"line\u001b[31m red\u001b[0m"}],"truncated":false}`))
	}))
	defer server.Close()

	oldURL, oldToken := apiURL, apiToken
	apiURL, apiToken = server.URL, "cli-token"
	t.Cleanup(func() { apiURL, apiToken = oldURL, oldToken })

	// Mirror the production flag types: newTestCmd declares everything as
	// string, but --limit and --offset are Int flags on the real command.
	cmd := &cobra.Command{Use: "logs", RunE: runObservabilityLogs}
	cmd.Flags().String("range", "1h", "")
	cmd.Flags().String("service", "demo/web", "")
	cmd.Flags().String("severity", "", "")
	cmd.Flags().String("search", "", "")
	cmd.Flags().Int("limit", 50, "")
	cmd.Flags().Int("offset", 0, "")
	cmd.Flags().Bool("json", true, "")
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "TIMESTAMP") || strings.Contains(out.String(), "telemetry unavailable") {
		t.Fatalf("JSON output included table/status text: %q", out.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON output = %q: %v", out.String(), err)
	}
	if decoded["status"] != "available" || !strings.Contains(out.String(), "line red") {
		t.Fatalf("decoded JSON = %#v", decoded)
	}
}

func TestObservabilityLogsRejectsInvalidRangeBeforeBackend(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	oldURL, oldToken := apiURL, apiToken
	apiURL, apiToken = server.URL, "cli-token"
	t.Cleanup(func() { apiURL, apiToken = oldURL, oldToken })

	cmd, _, _ := newTestCmd("logs", map[string]string{
		"range": "48h", "service": "", "severity": "", "search": "", "limit": "50", "offset": "0",
	}, runObservabilityLogs)
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, nil); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("invalid range error = %v", err)
	}
	if hit {
		t.Fatal("invalid range reached the backend")
	}
}

func TestObservabilityTableOutputStripsTerminalControls(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	result := observability.Result{
		Status: observability.StatusAvailable,
		Rows:   []observability.Row{{"p_timestamp": "2026-10-20T12:00:00Z", "severity_text": "error", "service.name": "demo", "body": "\x1b[31mboom\x1b[0m\nnext"}},
	}
	if err := printLogs(cmd, result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "\nnext") {
		t.Fatalf("unsafe table output = %q", out.String())
	}
}
