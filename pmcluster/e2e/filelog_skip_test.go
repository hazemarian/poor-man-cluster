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
//	container label → docker_observer → per-container filelog receiver →
//	resource attribute service.name → Parseable row
//
// Two containers emit a unique marker to stdout every second. One carries
// io.pmcluster.skip_filelog=true. The assertion is BOTH directions: the kept
// marker must be queryable in Parseable, and the skipped marker must not be
// there at all. A one-sided assertion would pass on an empty stream, which is
// exactly how the old single-receiver design looked healthy while ingesting
// zero lines of container output.
//
// Deliberately plain `docker run`, not `docker compose`: the runner that owns
// this suite has no working compose plugin, so compose-based e2e tests skip
// here while looking green.
func TestE2EFilelogAttributionAndSkip(t *testing.T) {
	requireDockerDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	dir := t.TempDir()
	user := "ops@example.com"
	pass := "AdminPass123!"
	stream := "pmcluster-logs"
	netName := "pmc-filelog-e2e"
	parseableName := "observability_parseable"
	keptName := "pmc-filelog-kept"
	skippedName := "pmc-filelog-skipped"
	collectorName := "pmc-filelog-collector"
	keptMarker := "KEPT_STDOUT_MARKER_7f3a"
	skippedMarker := "SKIPPED_STDOUT_MARKER_9c21"
	pport := hostPort(t)

	run := func(args ...string) (string, error) {
		var buf bytes.Buffer
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err := cmd.Run()
		return buf.String(), err
	}
	mustRun := func(what string, args ...string) {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", what, err, out)
		}
	}
	remove := func(name string) {
		clean, cancelClean := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancelClean()
		_ = exec.CommandContext(clean, "docker", "rm", "-f", name).Run()
	}

	remove(parseableName)
	remove(collectorName)
	remove(keptName)
	remove(skippedName)
	_ = exec.CommandContext(ctx, "docker", "network", "rm", netName).Run()
	t.Cleanup(func() {
		remove(parseableName)
		remove(collectorName)
		remove(keptName)
		remove(skippedName)
		clean, cancelClean := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancelClean()
		_ = exec.CommandContext(clean, "docker", "network", "rm", netName).Run()
	})

	mustRun("docker network create", "network", "create", netName)

	t.Log("Starting Parseable...")
	// The argv is the WHOLE command: this image declares no ENTRYPOINT, so a
	// bare `local-store` argument would replace the binary and fail to exec.
	parseableOut, err := run("run", "-d", "--name", parseableName,
		"--network", netName, "-p", fmt.Sprintf("%d:8000", pport),
		"-e", "P_ADDR=0.0.0.0:8000",
		"-e", "P_USERNAME="+user,
		"-e", "P_PASSWORD="+pass,
		"-e", "P_FS_DIR=/data",
		"-e", "P_STAGING_DIR=/staging",
		"-e", "P_INDEX_DIR=/data/index",
		"-e", "P_SEND_ANONYMOUS_USAGE_DATA=false",
		"-e", "P_CHECK_UPDATE=false",
		"parseable/parseable:v2.8.0", "/usr/bin/parseable", "local-store")
	if err != nil {
		t.Fatalf("docker run parseable: %v\n%s", err, parseableOut)
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
	configPath := filepath.Join(dir, "otel-config.yaml")
	if err := os.WriteFile(configPath, rendered, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Log("Starting the collector with the rendered config...")
	collectorOut, err := run("run", "-d", "--name", collectorName, "--network", netName,
		// The collector reads the host's Docker API and the host's container log
		// directory, exactly as the production Swarm service does.
		"-u", "0:0",
		"-v", configPath+":/etc/otel-config.yaml:ro",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", "/var/lib/docker/containers:/var/lib/docker/containers:ro",
		"otel/opentelemetry-collector-contrib:0.157.0",
		"--config=/etc/otel-config.yaml")
	if err != nil {
		t.Fatalf("docker run collector: %v\n%s", err, collectorOut)
	}

	t.Log("Starting the two emitters (one opted out)...")
	// Compose-style identity labels are what the receiver maps to
	// <project>_<service>; a Swarm service carries the swarm labels instead.
	emitter := func(name, service, marker string, skip bool) {
		args := []string{"run", "-d", "--name", name, "--network", netName,
			"--label", "com.docker.compose.project=demoe2e",
			"--label", "com.docker.compose.service=" + service}
		if skip {
			args = append(args, "--label", "io.pmcluster.skip_filelog=true")
		}
		args = append(args, "alpine:3.20", "sh", "-c",
			fmt.Sprintf("while true; do echo %s; sleep 1; done", marker))
		out, err := run(args...)
		if err != nil {
			t.Fatalf("docker run %s: %v\n%s", name, err, out)
		}
	}
	emitter(keptName, "kept", keptMarker, false)
	emitter(skippedName, "skipped", skippedMarker, true)

	// The kept marker must appear; the skipped one must never appear. Poll
	// rather than sleep a fixed time so a fast host does not wait and a slow one
	// is not judged early.
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
		logs, _ := run("logs", "--tail", "200", collectorName)
		t.Fatalf("kept container's stdout never reached Parseable: no filelog attribution.\nCollector log tail:\n%s", tail(logs, 4000))
	}
	t.Logf("✅ %s attributed and ingested (service identity + filelog path work)", keptMarker)

	if parseableContains(t, ctx, pport, user, pass, stream, skippedMarker) {
		t.Fatalf("%s reached Parseable although the container carried io.pmcluster.skip_filelog=true — the opt-out does not work", skippedMarker)
	}
	t.Logf("✅ %s absent: skip_filelog withholds a service's logs", skippedMarker)

	// A config that fails to start must never ship; it would crash-loop the
	// collector on every node at once.
	logs, _ := run("logs", "--tail", "200", collectorName)
	for _, bad := range []string{"Error:", "failed to start", "invalid configuration"} {
		if strings.Contains(logs, bad) {
			t.Errorf("collector reported %q:\n%s", bad, tail(logs, 4000))
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
