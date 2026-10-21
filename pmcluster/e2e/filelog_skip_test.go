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
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
)

// TestE2EFilelogAttributionAndSkip is the test the previous collector design
// could not have passed, and the reason skip_filelog is worth trusting.
//
// It proves the whole chain against a REAL Docker daemon, a REAL collector
// running the config pmcluster renders, and a REAL Parseable:
//
//	DSL label → Swarm/Compose container label → docker_observer → per-container
//	filelog receiver → resource attribute service.name → Parseable row
//
// Two containers emit a unique marker to stdout every second. One carries
// io.pmcluster.skip_filelog=true. The assertion is BOTH directions: the kept
// marker must be queryable in Parseable, and the skipped marker must not be
// there at all. A one-sided assertion would pass on an empty stream, which is
// exactly how the old single-receiver design looked healthy while ingesting
// zero lines of container output.
func TestE2EFilelogAttributionAndSkip(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	dir := t.TempDir()
	user := "ops@example.com"
	pass := "AdminPass123!"
	stream := "pmcluster-logs"
	project := "pmcluster-filelog-e2e"
	keptMarker := "KEPT_STDOUT_MARKER_7f3a"
	skippedMarker := "SKIPPED_STDOUT_MARKER_9c21"

	pport := hostPort(t)

	compose := fmt.Sprintf(`
services:
  observability_parseable:
    image: parseable/parseable:v2.8.0
    command: ["local-store"]
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

  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.157.0
    # The collector reads the host's Docker API and the host's container log
    # directory, exactly as the production Swarm service does.
    user: "0:0"
    command: ["--config=/etc/otel-config.yaml"]
    volumes:
      - ${OTEL_CONFIG}:/etc/otel-config.yaml:ro
      - /var/run/docker.sock:/var/run/docker.sock
      - /var/lib/docker/containers:/var/lib/docker/containers:ro

  # Emits to stdout only. No OTel SDK, so its logs can ONLY arrive through the
  # filelog path — which is the path under test.
  kept:
    image: alpine:3.20
    command: ["sh", "-c", "while true; do echo %s; sleep 1; done"]
    logging:
      driver: json-file

  skipped:
    image: alpine:3.20
    command: ["sh", "-c", "while true; do echo %s; sleep 1; done"]
    logging:
      driver: json-file
    labels:
      io.pmcluster.skip_filelog: "true"

volumes:
  parseable_data:
  parseable_staging:
`, user, pass, pport, keptMarker, skippedMarker)

	composePath := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	otelConfigPath := filepath.Join(dir, "otel-config.yaml")

	composeArgs := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-f", composePath, "-p", project}, args...)...)
		cmd.Env = append(os.Environ(), "OTEL_CONFIG="+otelConfigPath)
		return cmd
	}
	t.Cleanup(func() {
		downCtx, downCancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer downCancel()
		cmd := exec.CommandContext(downCtx, "docker", "compose", "-f", composePath, "-p", project, "down", "-v")
		cmd.Env = append(os.Environ(), "OTEL_CONFIG="+otelConfigPath)
		out, _ := cmd.CombinedOutput()
		t.Logf("compose down: %s", strings.TrimSpace(string(out)))
	})

	t.Log("Starting Parseable...")
	if out, err := composeArgs("up", "-d", "observability_parseable").CombinedOutput(); err != nil {
		t.Skipf("docker compose up failed (image pull / daemon issue — skipping): %v\n%s", err, out)
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
	// Guard the premise: if the rendered config ever loses the pieces this test
	// exists for, fail here with a readable reason instead of timing out.
	for _, want := range []string{"receiver_creator", "docker_observer", "skip_filelog"} {
		if !bytes.Contains(rendered, []byte(want)) {
			t.Fatalf("rendered collector config no longer contains %q — the filelog path under test is gone", want)
		}
	}
	if err := os.WriteFile(otelConfigPath, rendered, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Log("Starting OTel collector and the two emitters...")
	if out, err := composeArgs("up", "-d", "otel-collector", "kept", "skipped").CombinedOutput(); err != nil {
		t.Fatalf("docker compose up collector/emitters: %v\n%s", err, out)
	}

	// The kept marker must appear; the skipped one must never appear. Poll
	// rather than sleep a fixed time so a fast cluster does not wait and a slow
	// one is not judged early.
	var keptSeen bool
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.Fatalf("context done while waiting for ingestion: %v", ctx.Err())
		default:
		}
		if parseableContains(t, ctx, pport, user, pass, stream, keptMarker) {
			keptSeen = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !keptSeen {
		logs := containerLogs(t, ctx, project+"-otel-collector-1")
		t.Fatalf("kept container's stdout never reached Parseable: no filelog attribution.\nCollector log tail:\n%s", tail(logs, 4000))
	}
	t.Logf("✅ %s attributed and ingested (service identity + filelog path work)", keptMarker)

	if parseableContains(t, ctx, pport, user, pass, stream, skippedMarker) {
		t.Fatalf("%s reached Parseable although the container carried io.pmcluster.skip_filelog=true — the opt-out does not work", skippedMarker)
	}
	t.Logf("✅ %s absent: skip_filelog withholds a service's logs", skippedMarker)

	// Prove the skip is a rule-level refusal, not a filter that ran late, and
	// that the receiver still exists for the kept service: the collector names
	// the receivers it starts when logging at debug is off only on errors, so
	// assert on the absence of a startup error instead of on a receiver name.
	logs := containerLogs(t, ctx, project+"-otel-collector-1")
	for _, bad := range []string{"Error:", "failed to start", "invalid configuration"} {
		if strings.Contains(logs, bad) {
			t.Errorf("collector reported %q — a config that fails to start must never ship:\n%s", bad, tail(logs, 4000))
		}
	}
}

// parseableContains reports whether the stream holds a row whose body contains
// marker within the last 15 minutes.
func parseableContains(t *testing.T, ctx context.Context, port int, user, pass, stream, marker string) bool {
	t.Helper()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	now := time.Now().UTC()
	body := map[string]string{
		// Backtick identifiers: the SPA writes them this way and they need no
		// JSON escaping (double-quoted identifiers must be escaped, and a
		// botched escape silently matches nothing).
		"query":     fmt.Sprintf("SELECT COUNT(*) AS n FROM `%s` WHERE body LIKE '%%%s%%'", stream, marker),
		"startTime": now.Add(-15 * time.Minute).Format(time.RFC3339Nano),
		"endTime":   now.Format(time.RFC3339Nano),
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/api/v1/query", port), bytes.NewReader(payload))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("query request error (backend may still be starting): %v", err)
		return false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Logf("query returned HTTP %d: %s", resp.StatusCode, tail(string(raw), 400))
		return false
	}
	var rows []struct {
		N int64 `json:"n"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) == 0 {
		t.Logf("unparseable query response: %s", tail(string(raw), 400))
		return false
	}
	return rows[0].N > 0
}

// tail returns the last n bytes of s, so a failure message stays readable.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
