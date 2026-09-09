//go:build e2e

package e2e

import (
	"bytes"
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

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
)

// TestE2EOpenObserveTokenIngestion validates the dedicated ingestion-token
// decoupling end-to-end against a REAL OpenObserve + REAL OTel collector:
//
//  1. Boot OpenObserve (v0.92.2) with NO root-token env — root email/password
//     only. This mirrors the new design: we do NOT bake a token into the OO
//     data volume.
//  2. Create a dedicated INGESTION token via the OO API
//     (POST /api/{org}/ingestion-tokens → an o2oi_... token), exactly as
//     pmcluster's provisioner does.
//  3. Render the collector config with the SAME function pmcluster uses
//     (RenderOTelCollectorConfig → exporter reads
//     `Authorization: Basic base64(<org>:<ingestion_token>)` at openobserve:5081),
//     then start the collector.
//  4. Push one OTLP/HTTP log into the collector and assert it was ingested via
//     OO's search API; assert the collector logged no auth/export error.
//
// A negative case sends a bogus credential and asserts OpenObserve rejects it
// (401/403), proving ingestion auth is enforced by the token.
func TestE2EOpenObserveTokenIngestion(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()

	dir := t.TempDir()
	email := "ops@example.com"
	pass := "AdminPass123!"
	marker := "OO_TOKEN_E2E_MARKER_42"
	project := "oo-token-e2e"
	// Use a free host port — an existing local OpenObserve may already hold 5080.
	ooPort := hostPort(t)
	collPort := hostPort(t)

	compose := fmt.Sprintf(`
services:
  openobserve:
    image: public.ecr.aws/zinclabs/openobserve:v0.92.2
    environment:
      - ZO_DATA_DIR=/data
      - ZO_HTTP_PORT=5080
      - ZO_GRPC_PORT=5081
      - ZO_ROOT_USER_EMAIL=%s
      - ZO_ROOT_USER_PASSWORD=%s
      # Deliberately NO ZO_ROOT_USER_TOKEN: OpenObserve caches root creds in its
      # data volume on first boot and ignores env after. The collector instead
      # uses a dedicated ingestion token created via the API and rendered into
      # the collector config (pmcluster keeps pass + token in its own config).
    volumes:
      - oo_data:/data
    ports:
      - "%d:5080"
    # No Docker healthcheck: the OpenObserve image is distroless (no shell), so
    # a CMD-SHELL probe can never run. The test polls /healthz from the host
    # instead (waitOOHealthy).

  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.157.0
    # The collector's filelog/docker_stats receivers + docker resourcedetection
    # need the host docker API and container log dirs, and must run as root to
    # read /var/run/docker.sock — same as the real production Swarm service.
    user: "0:0"
    command: ["--config=/etc/otel-config.yaml"]
    # Started manually AFTER OpenObserve is healthy so the config (which embeds
    # the freshly-minted ingestion token) exists before the collector boots.
    ports:
      - "%d:4318"
    volumes:
      - ${OTEL_CONFIG}:/etc/otel-config.yaml:ro
      # docker.sock only (for docker_stats + docker resourcedetection). We do
      # NOT mount /var/lib/docker/containers: doing so makes the filelog
      # receiver scrape the host's real Swarm container logs into OO's default
      # stream, mixing its schema (a string "stream" attr vs our int severity)
      # and breaking the search — obscuring the OTLP marker this test asserts.
      - /var/run/docker.sock:/var/run/docker.sock

volumes:
  oo_data:
`, email, pass, ooPort, collPort)

	composePath := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	otelConfigPath := filepath.Join(dir, "otel-config.yaml")

	// Register cleanup BEFORE `up` so even a partial failure tears everything down.
	t.Cleanup(func() {
		downCtx, downCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer downCancel()
		cmd := exec.CommandContext(downCtx, "docker", "compose", "-f", composePath, "-p", project, "down", "-v")
		cmd.Env = append(os.Environ(), "OTEL_CONFIG="+otelConfigPath)
		out, _ := cmd.CombinedOutput()
		t.Logf("compose down: %s", strings.TrimSpace(string(out)))
	})

	// ── 2a. Boot OpenObserve only, and wait until it's healthy. ────────────
	t.Log("Starting OpenObserve...")
	upCtx, upCancel := context.WithTimeout(ctx, 6*time.Minute)
	defer upCancel()
	upCmd := exec.CommandContext(upCtx, "docker", "compose", "-f", composePath, "-p", project, "up", "-d", "openobserve")
	upCmd.Env = append(os.Environ(), "OTEL_CONFIG="+otelConfigPath)
	upOut, err := upCmd.CombinedOutput()
	if err != nil {
		t.Skipf("docker compose up failed (image pull / daemon issue — skipping): %v\n%s", err, upOut)
	}
	waitOOHealthy(t, ctx, ooPort, 240*time.Second)

	// ── 2b. Mint a dedicated INGESTION token via the OO API (as pmcluster's
	//        provisioner does), then render the collector config with it. ──
	token := createIngestionToken(t, ctx, ooPort, email, pass)
	rendered, err := cluster.RenderOTelCollectorConfig(cluster.RenderInput{
		Domain:                    "localhost",
		OpenObserveOrg:            "default",
		OpenObserveIngestionToken: token,
	})
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	if err := os.WriteFile(otelConfigPath, rendered, 0o644); err != nil {
		t.Fatal(err)
	}

	// ── 2c. Now boot the collector (its config now embeds the real token). ─
	t.Log("Starting OTel collector...")
	collCtx, collCancel := context.WithTimeout(ctx, 6*time.Minute)
	defer collCancel()
	collCmd := exec.CommandContext(collCtx, "docker", "compose", "-f", composePath, "-p", project, "up", "-d", "otel-collector")
	collCmd.Env = append(os.Environ(), "OTEL_CONFIG="+otelConfigPath)
	if out, err := collCmd.CombinedOutput(); err != nil {
		t.Fatalf("docker compose up collector: %v\n%s", err, out)
	}

	// ── 3. Deliver the marker log to the collector ─────────────────────────
	postOTLPToCollector(t, ctx, collPort, marker)
	time.Sleep(8 * time.Second) // let the collector export + OO index

	// ── 4. Assert the marker was ingested (query OO's search API) ───────────
	// Note: OO's REST search API authenticates with the human PASSWORD (the
	// token only authenticates OTLP ingestion) and requires a start_time window.
	ingested, body := searchOpenObserve(t, ctx, ooPort, pass, "select * from \"default\" ORDER BY _timestamp DESC", 15, 5)
	if !ingested {
		t.Errorf("OO did NOT ingest the marker %q (search via root admin password).\nSearch body:\n%s", marker, body)
	} else if !strings.Contains(body, marker) {
		t.Errorf("OO search did not contain marker %q.\nSearch body:\n%s", marker, body)
	} else {
		t.Logf("✅ OO ingested the log via Basic base64(default:ingestion_token)")
	}

	// Assert the collector logged no export auth error.
	collectorLogs := containerLogs(t, ctx, project+"-otel-collector-1")
	for _, bad := range []string{"401", "403", "PermissionDenied", "Unauthenticated", "Failed to export"} {
		if strings.Contains(collectorLogs, bad) {
			t.Errorf("collector exporter reported an auth/export error (%q) with the valid token:\n%s", bad, collectorLogs)
		}
	}

	// ── 5. Negative: auth still enforced — a bogus credential is rejected ──
	// Note: OO's HTTP OTLP endpoint accepts EITHER root credential (token or
	// password), so we CANNOT assert "password is rejected" here. What we can
	// assert — and what proves auth is on — is that a credential matching
	// neither root password nor node token is rejected with 401/403. The real
	// decoupling guarantee (the collector's gRPC path REQUIRES the token, so a
	// password rotation never invalidates the collector config) is covered
	// upstream by unit tests + the positive ingestion above.
	if ok, code := postOTLPToOO(t, ctx, ooPort, email, "definitely-not-a-root-cred3ntial", "oops-wrong-cred"); ok {
		t.Errorf("OpenObserve ACCEPTED (HTTP %d) an OTLP ingest with a bogus credential — auth is not enforced", code)
	} else {
		t.Logf("✅ OpenObserve rejected (HTTP %d) ingestion with a bogus credential", code)
	}

	t.Log("=== OpenObserve token ingestion E2E complete ===")
}

// waitOOHealthy polls OpenObserve's /healthz endpoint from the host until it
// returns 2xx (or the timeout/context elapses). The OO image is distroless (no
// shell), so there is no Docker healthcheck — the test must wait this way.
func waitOOHealthy(t *testing.T, ctx context.Context, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.Fatalf("context done waiting for OpenObserve healthy: %v", ctx.Err())
		default:
		}
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				t.Logf("OpenObserve healthy (HTTP %d)", resp.StatusCode)
				return
			}
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("timed out waiting for OpenObserve /healthz at %s", url)
}

// searchOpenObserve queries OO's search API authenticated with the root admin
// PASSWORD (its REST search API does not accept the ingestion token — token vs
// password are separate auth surfaces in OO). OO also requires a start/end time
// window on timestamp queries, so a wide window is added. Returns whether the
// query returned 2xx and the response body.
func searchOpenObserve(t *testing.T, ctx context.Context, port int, password, sql string, from, size int) (bool, string) {
	t.Helper()
	now := time.Now().UnixMicro()
	payload, _ := json.Marshal(map[string]any{
		"query": map[string]any{
			"sql":        sql,
			"start_time": now - 24*60*60*1_000_000, // 24h ago
			"end_time":   now + 60*1_000_000,       // a minute ahead
		},
		"from": from, "size": size,
	})
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("ops@example.com:"+password))

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST", fmt.Sprintf("http://127.0.0.1:%d/api/default/_search", port), bytes.NewReader(payload))
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("search request error (may still be starting): %v", err)
		return false, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300, string(body)
}

// createIngestionToken mints a dedicated OO ingestion token via the API
// (POST /api/{org}/ingestion-tokens) authenticated with the root admin —
// exactly the call pmcluster's OpenObserveProvisioner makes.
func createIngestionToken(t *testing.T, ctx context.Context, port int, rootEmail, rootPass string) string {
	t.Helper()
	payload := []byte(`{"name":"pmcluster-e2e","description":"e2e ingestion token"}`)
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST", fmt.Sprintf("http://127.0.0.1:%d/api/default/ingestion-tokens", port), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create token request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(rootEmail+":"+rootPass)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create ingestion token: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create ingestion token: HTTP %d: %s", resp.StatusCode, body)
	}
	var parsed struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if parsed.Data.Token == "" {
		t.Fatalf("ingestion token response missing token: %s", body)
	}
	t.Logf("minted ingestion token (%d chars, prefix o2oi_)", len(parsed.Data.Token))
	return parsed.Data.Token
}

// postOTLPToOO sends a bare OTLP/HTTP log to OO with the given credentials,
// returning success + HTTP code. OO 0.92.2 exposes an OTLP/HTTP endpoint at
// /api/{org}/v1/logs for convenience.
func postOTLPToOO(t *testing.T, ctx context.Context, port int, email, password, marker string) (bool, int) {
	t.Helper()
	ts := time.Now().UnixNano()
	body := fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"oo-token-e2e-neg"}}]},"scopeLogs":[{"scope":{"name":"test"},"logRecords":[{"timeUnixNano":%d,"severityNumber":9,"severityText":"INFO","body":{"stringValue":"%s"}}]}]}]}`, ts, marker)
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+password))

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST", fmt.Sprintf("http://127.0.0.1:%d/api/default/v1/logs", port), strings.NewReader(body))
	if err != nil {
		return false, 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
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
// starting even after `docker compose up` returns (only OO is health-gated).
func postOTLPToCollector(t *testing.T, ctx context.Context, port int, marker string) {
	t.Helper()
	ts := time.Now().UnixNano()
	body := fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"oo-token-e2e"}}]},"scopeLogs":[{"scope":{"name":"test"},"logRecords":[{"timeUnixNano":%d,"severityNumber":9,"severityText":"INFO","body":{"stringValue":"%s"}}]}]}]}`, ts, marker)
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

// containerLogs returns the combined logs for a numeric-named compose
// container (e.g. "<project>-<service>-1").
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
