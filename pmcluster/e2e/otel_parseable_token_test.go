//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
)

// TestE2EParseableAdminAuthIngestion validates the admin-scoped ingestion model
// end-to-end against a REAL Parseable + REAL OTel collector:
//
//  1. Boot Parseable (v2.8.0) with P_USERNAME + P_PASSWORD — the same way the
//     observability stack runs it. Parseable reads these on EVERY boot, so the
//     configured credential always applies (this is the property that
//     OpenObserve lacked: it seeded its root user once into a metadata volume
//     and then silently ignored the configured password).
//  2. Render the collector config with the SAME function pmcluster uses
//     (RenderOTelCollectorConfig → the Parseable exporters carry
//     `Authorization: Basic <base64(user:password)>` and the mandatory
//     X-P-Stream / X-P-Log-Source headers), then start the collector.
//  3. Push one OTLP/HTTP log into the collector and assert Parseable created
//     the stream; assert the collector logged no export error.
//
// A negative case sends a bogus credential straight to Parseable's OTLP
// endpoint and asserts it is rejected (401/403), proving ingestion auth is
// enforced.
//
// The Parseable service MUST be named observability_parseable: the rendered
// collector config addresses it as http://observability_parseable:8000, so the
// compose service name has to match for DNS to resolve inside the network.
func TestE2EParseableAdminAuthIngestion(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()

	dir := t.TempDir()
	user := "ops@example.com"
	pass := "AdminPass123!"
	marker := "PARSEABLE_E2E_MARKER_42"
	stream := "pmcluster-logs"
	project := "parseable-e2e"

	pport := hostPort(t)
	collPort := hostPort(t)

	compose := fmt.Sprintf(`
services:
  observability_parseable:
    image: parseable/parseable:v2.8.0
    command: ["/usr/bin/parseable", "local-store"]
    environment:
      - P_ADDR=0.0.0.0:8000
      - P_USERNAME=%s
      - P_PASSWORD=%s
      - P_FS_DIR=/data
      - P_STAGING_DIR=/staging
      - P_INDEX_DIR=/data/index
      - P_SEND_ANONYMOUS_USAGE_DATA=false
      - P_CHECK_UPDATE=false
    volumes:
      - parseable_data:/data
      - parseable_staging:/staging
    ports:
      - "%d:8000"
    # No Docker healthcheck: the Parseable image is distroless (no shell), so a
    # CMD-SHELL probe can never run. The test polls / from the host instead.

  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.157.0
    # The collector's filelog/docker_stats receivers + docker resourcedetection
    # need the host docker API and must run as root to read /var/run/docker.sock
    # — same as the real production Swarm service.
    user: "0:0"
    command: ["--config=/etc/otel-config.yaml"]
    # Started manually AFTER Parseable is healthy so the config exists first.
    ports:
      - "%d:4318"
    volumes:
      - ${OTEL_CONFIG}:/etc/otel-config.yaml:ro
      # docker.sock only (for docker_stats + docker resourcedetection). We do
      # NOT mount /var/lib/docker/containers: doing so makes the filelog
      # receiver scrape the host's real Swarm container logs, which would bury
      # the OTLP marker this test asserts.
      - /var/run/docker.sock:/var/run/docker.sock

volumes:
  parseable_data:
  parseable_staging:
`, user, pass, pport, collPort)

	composePath := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	otelConfigPath := filepath.Join(dir, "otel-config.yaml")

	t.Cleanup(func() {
		downCtx, downCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer downCancel()
		cmd := exec.CommandContext(downCtx, "docker", "compose", "-f", composePath, "-p", project, "down", "-v")
		cmd.Env = append(os.Environ(), "OTEL_CONFIG="+otelConfigPath)
		out, _ := cmd.CombinedOutput()
		t.Logf("compose down: %s", strings.TrimSpace(string(out)))
	})

	t.Log("Starting Parseable...")
	upCtx, upCancel := context.WithTimeout(ctx, 6*time.Minute)
	defer upCancel()
	upCmd := exec.CommandContext(upCtx, "docker", "compose", "-f", composePath, "-p", project, "up", "-d", "observability_parseable")
	upCmd.Env = append(os.Environ(), "OTEL_CONFIG="+otelConfigPath)
	upOut, err := upCmd.CombinedOutput()
	if err != nil {
		t.Skipf("docker compose up failed (image pull / daemon issue — skipping): %v\n%s", err, upOut)
	}
	waitParseableHealthy(t, ctx, pport, 240*time.Second)

	rendered, err := cluster.RenderOTelCollectorConfig(cluster.RenderInput{
		Domain: "localhost",
		ObservabilityBasicAuth: "Basic " +
			base64.StdEncoding.EncodeToString([]byte(user+":"+pass)),
	})
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	if err := os.WriteFile(otelConfigPath, rendered, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Log("Starting OTel collector...")
	collCtx, collCancel := context.WithTimeout(ctx, 6*time.Minute)
	defer collCancel()
	collCmd := exec.CommandContext(collCtx, "docker", "compose", "-f", composePath, "-p", project, "up", "-d", "otel-collector")
	collCmd.Env = append(os.Environ(), "OTEL_CONFIG="+otelConfigPath)
	if out, err := collCmd.CombinedOutput(); err != nil {
		t.Fatalf("docker compose up collector: %v\n%s", err, out)
	}

	postOTLPToCollector(t, ctx, collPort, marker)
	time.Sleep(10 * time.Second)

	ok, body := parseableHasStream(t, ctx, pport, user, pass, stream)
	if ok {
		t.Logf("✅ Parseable created stream %q from collector ingestion", stream)
	} else {
		t.Errorf("Parseable did NOT expose stream %q — collector ingestion failed.\nStream list:\n%s", stream, body)

		collectorLogs := containerLogs(t, ctx, project+"-otel-collector-1")
		for _, bad := range []string{"401", "403", "PermissionDenied", "Unauthenticated", "Exporting failed"} {
			if strings.Contains(collectorLogs, bad) {
				t.Errorf("collector exporter reported an error (%q) with valid credentials:\n%s", bad, collectorLogs)
			}
		}
	}

	// The negative case: a bogus credential must be refused. Parseable requires
	// the X-P-Stream / X-P-Log-Source headers, so send them — the ONLY thing
	// wrong is the credential.
	if accepted, code := postOTLPToParseable(t, ctx, pport, user, "definitely-not-a-valid-cred3ntial", "oops-wrong-cred"); accepted {
		t.Errorf("Parseable ACCEPTED (HTTP %d) an OTLP ingest with a bogus credential — auth is not enforced", code)
	} else {
		t.Logf("✅ Parseable rejected (HTTP %d) ingestion with a bogus credential", code)
	}

	t.Log("=== Parseable admin-auth ingestion E2E complete ===")
}

// waitParseableHealthy polls Parseable's UI root from the host until it returns
// 2xx. The image is distroless (no shell) so there is no Docker healthcheck.
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
func parseableHasStream(t *testing.T, ctx context.Context, port int, user, pass, stream string) (bool, string) {
	t.Helper()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET",
		fmt.Sprintf("http://127.0.0.1:%d/api/v1/logstream", port), nil)
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("stream list request error (may still be starting): %v", err)
		return false, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, body)
	}
	// The endpoint returns a JSON array of stream objects; match on the name
	// field rather than substring-matching the whole document so a marker in
	// another field cannot produce a false pass.
	var streams []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &streams); err != nil {
		return false, fmt.Sprintf("unparseable stream list: %v\n%s", err, body)
	}
	for _, s := range streams {
		if s.Name == stream {
			return true, string(body)
		}
	}
	return false, string(body)
}

// postOTLPToParseable sends a bare OTLP/HTTP log directly to Parseable with the
// given credential, returning success + HTTP code. The X-P-Stream / X-P-Log-Source
// headers are mandatory on every signal, so the only variable is the credential.
func postOTLPToParseable(t *testing.T, ctx context.Context, port int, user, password, marker string) (bool, int) {
	t.Helper()
	ts := time.Now().UnixNano()
	body := fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"parseable-e2e-neg"}}]},"scopeLogs":[{"scope":{"name":"test"},"logRecords":[{"timeUnixNano":%d,"severityNumber":9,"severityText":"INFO","body":{"stringValue":"%s"}}]}]}]}`, ts, marker)
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST",
		fmt.Sprintf("http://127.0.0.1:%d/v1/logs", port), strings.NewReader(body))
	if err != nil {
		return false, 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	req.Header.Set("X-P-Stream", "pmcluster-logs")
	req.Header.Set("X-P-Log-Source", "otel-logs")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("negative ingest request error: %v", err)
		return false, 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300, resp.StatusCode
}

// hostPort returns a currently-free host TCP port number, reusing the
// package-level freePort (which returns "127.0.0.1:<port>") and parsing it.
func hostPort(t *testing.T) int {
	t.Helper()
	addr := freePort(t)
	parts := strings.Split(addr, ":")
	if len(parts) != 2 {
		t.Fatalf("unexpected freePort address %q", addr)
	}
	p, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("parse freePort address %q: %v", addr, err)
	}
	return p
}

// postOTLPToCollector sends a single OTLP/HTTP log to the OTel collector's
// published 4318 port (no auth — the collector's OTLP receiver is open). It
// retries for up to ~90s because the collector's OTLP receiver can still be
// starting even after `docker compose up` returns.
func postOTLPToCollector(t *testing.T, ctx context.Context, port int, marker string) {
	t.Helper()
	ts := time.Now().UnixNano()
	body := fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"parseable-e2e"}}]},"scopeLogs":[{"scope":{"name":"test"},"logRecords":[{"timeUnixNano":%d,"severityNumber":9,"severityText":"INFO","body":{"stringValue":"%s"}}]}]}]}`, ts, marker)
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/v1/logs", port)

	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.Fatalf("context done posting OTLP to collector: %v", ctx.Err())
		default:
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(cctx, "POST", endpoint, strings.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			resp, derr := http.DefaultClient.Do(req)
			if derr == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode < 300 {
					cancel()
					t.Logf("sent marker %q to collector (HTTP %d)", marker, resp.StatusCode)
					return
				}
				lastErr = fmt.Errorf("collector rejected OTLP with HTTP %d", resp.StatusCode)
			} else {
				lastErr = derr
			}
		} else {
			lastErr = err
		}
		cancel()
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("collector never accepted OTLP: %v", lastErr)
}

// containerLogs returns the combined logs for a compose container.
func containerLogs(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "logs", name).CombinedOutput()
	if err != nil {
		t.Logf("docker logs %s: %v", name, err)
	}
	return string(out)
}
