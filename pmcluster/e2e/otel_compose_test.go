//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOtelComposeLogs validates the OTel collector pipeline outside of Swarm
// using docker compose. It tests:
//  1. filelog receiver → reads JSON log lines written to a shared file
//  2. OTLP receiver → accepts OTLP log export from a test container
//
// Both pipelines export to debug exporter; we verify emitted log content.
// No Swarm/VXLAN needed — runs on macOS too.
func TestOtelComposeLogs(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dir := t.TempDir()

	otelConfig := `
receivers:
  filelog:
    include:
      - /logs/*.log
    start_at: beginning
    operators:
      - type: json_parser
        error_mode: ignore
        timestamp:
          parse_from: attributes.time
          layout: '%Y-%m-%dT%H:%M:%S.%LZ'
      - type: move
        from: attributes.log
        to: body
        if: attributes.log != nil
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:
    send_batch_size: 1
    timeout: 100ms

exporters:
  debug:
    verbosity: detailed

service:
  telemetry:
    logs:
      level: info
  pipelines:
    logs/filelog:
      receivers:  [filelog]
      processors: [batch]
      exporters:  [debug]
    logs/otlp:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [debug]
`
	if err := os.WriteFile(filepath.Join(dir, "otel-config.yaml"), []byte(otelConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	compose := `
services:
  stdout-app:
    image: alpine:3.21
    entrypoint: ["/bin/sh", "-c"]
    command:
      - |
        i=0
        while [ $$i -lt 5 ]; do
          i=$$((i+1))
          TS=$$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
          echo '{"log":"STDOUT_LOG_ENTRY_'"'"'$$i'"'"'_FROM_E2E_COMPOSE","stream":"stdout","time":"'"$$TS"'"}' >> /logs/app.log
          sleep 1
        done
        echo "stdout-app done writing. sleeping..."
        sleep 120
    volumes:
      - logs_vol:/logs

  otlp-app:
    image: alpine:3.21
    entrypoint: ["/bin/sh", "-c"]
    command:
      - |
        apk add --no-cache curl >/dev/null 2>&1
        # Wait for OTel collector OTLP endpoint to be ready
        for _ in 1 2 3 4 5 6 7 8 9 10; do
          curl -s -o /dev/null -w "%{http_code}" http://otel-collector:4318/v1/logs && break
          sleep 1
        done
        echo "OTLP endpoint ready, starting to send logs..."
        i=0
        while [ $$i -lt 5 ]; do
          i=$$((i+1))
          ts=$$(date +%s)000000000
          resp=$$(curl -s -w "\n%%{http_code}" -X POST http://otel-collector:4318/v1/logs \
            -H "Content-Type: application/json" \
            -d '{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"otlp-compose-test"}}]},"scopeLogs":[{"scope":{"name":"test"},"logRecords":[{"timeUnixNano":"'"$$ts"'","observedTimeUnixNano":"'"$$ts"'","severityNumber":9,"severityText":"INFO","body":{"stringValue":"OTLP_LOG_ENTRY_'"$$i"'_FROM_E2E_COMPOSE"}}]}]}]}')
          echo "Sent OTLP log #$$i: $$resp"
          sleep 1
        done
        echo "otlp-app done. sleeping..."
        sleep 120
    depends_on:
      otel-collector:
        condition: service_started

  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.157.0
    command: ["--config=/etc/otel-config.yaml"]
    volumes:
      - ${OTEL_CONFIG}:/etc/otel-config.yaml:ro
      - logs_vol:/logs:ro

volumes:
  logs_vol:
`

	composePath := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Log("Starting docker compose...")
	upCtx, upCancel := context.WithTimeout(ctx, 90*time.Second)
	defer upCancel()

	otelConfigPath := filepath.Join(dir, "otel-config.yaml")
	upCmd := exec.CommandContext(upCtx, "docker", "compose", "-f", composePath, "-p", "otel-e2e", "up", "-d")
	upCmd.Env = append(os.Environ(), "OTEL_CONFIG="+otelConfigPath)
	upOut, err := upCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose up: %v\n%s", err, upOut)
	}
	t.Logf("compose up: OK")

	t.Cleanup(func() {
		t.Log("Cleaning up docker compose...")
		downCtx, downCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer downCancel()
		cmd := exec.CommandContext(downCtx, "docker", "compose", "-f", composePath, "-p", "otel-e2e", "down", "-v")
		cmd.Env = append(os.Environ(), "OTEL_CONFIG="+otelConfigPath)
		out, _ := cmd.CombinedOutput()
		t.Logf("compose down: %s", strings.TrimSpace(string(out)))
	})

	t.Log("Waiting for test apps to write logs...")
	time.Sleep(20 * time.Second)

	logCtx, logCancel := context.WithTimeout(ctx, 15*time.Second)
	defer logCancel()

	logCmd := exec.CommandContext(logCtx, "docker", "logs", "otel-e2e-otel-collector-1")
	logOut, err := logCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs otel-collector: %v\n%s", err, string(logOut))
	}
	collectorLogs := string(logOut)

	if strings.Contains(collectorLogs, "STDOUT_LOG_ENTRY") {
		t.Log("✅ filelog pipeline: found STDOUT_LOG_ENTRY in collector output!")
	} else {
		t.Error("filelog pipeline: STDOUT_LOG_ENTRY NOT found in collector output")
		tail := collectorLogs
		if len(tail) > 3000 {
			tail = tail[len(tail)-3000:]
		}
		t.Logf("Collector logs tail:\n%s", tail)
	}

	if strings.Contains(collectorLogs, "OTLP_LOG_ENTRY") {
		t.Log("✅ OTLP pipeline: found OTLP_LOG_ENTRY in collector output!")
	} else {
		t.Error("OTLP pipeline: OTLP_LOG_ENTRY NOT found in collector output")
	}

	if strings.Contains(collectorLogs, "STDOUT_LOG_ENTRY_1_FROM_E2E_COMPOSE") {
		t.Log("✅ filelog: specific entry STDOUT_LOG_ENTRY_1 confirmed!")
	}
	if strings.Contains(collectorLogs, "OTLP_LOG_ENTRY_1_FROM_E2E_COMPOSE") {
		t.Log("✅ OTLP: specific entry OTLP_LOG_ENTRY_1 confirmed!")
	}

	t.Log("=== OTel Compose E2E Complete ===")
}

// TestOtelComposeExclude verifies that the filelog receiver's exclude
// patterns correctly skip containers. It runs a stdout-app that writes
// EXCLUDED log entries, but the OTel config excludes it via a path pattern.
// The collector should NOT see those entries.
func TestOtelComposeExclude(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dir := t.TempDir()

	otelConfig := `
receivers:
  filelog:
    include:
      - /logs/*.log
    # Simulates skip_filelog: true — exclude paths containing "excluded"
    exclude:
      - /logs/*excluded*.log
    start_at: beginning
    operators:
      - type: json_parser
        error_mode: ignore
      - type: move
        from: attributes.log
        to: body
        if: attributes.log != nil
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:
    send_batch_size: 1
    timeout: 100ms

exporters:
  debug:
    verbosity: detailed

service:
  telemetry:
    logs:
      level: info
  pipelines:
    logs/filelog:
      receivers:  [filelog]
      processors: [batch]
      exporters:  [debug]
    logs/otlp:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [debug]
`
	if err := os.WriteFile(filepath.Join(dir, "otel-config.yaml"), []byte(otelConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	compose := `
services:
  included-app:
    image: alpine:3.21
    entrypoint: ["/bin/sh", "-c"]
    command:
      - |
        i=0
        while [ $$i -lt 5 ]; do
          i=$$((i+1))
          echo '{"log":"INCLUDED_ENTRY_'"'"'$$i'"'"'","stream":"stdout"}' >> /logs/included.log
          sleep 1
        done
        echo "included-app done"
        sleep 120
    volumes:
      - logs_vol:/logs

  excluded-app:
    image: alpine:3.21
    entrypoint: ["/bin/sh", "-c"]
    command:
      - |
        i=0
        while [ $$i -lt 5 ]; do
          i=$$((i+1))
          echo '{"log":"EXCLUDED_ENTRY_'"'"'$$i'"'"'","stream":"stdout"}' >> /logs/excluded.log
          sleep 1
        done
        echo "excluded-app done"
        sleep 120
    volumes:
      - logs_vol:/logs

  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.157.0
    command: ["--config=/etc/otel-config.yaml"]
    volumes:
      - ${OTEL_CONFIG}:/etc/otel-config.yaml:ro
      - logs_vol:/logs:ro

volumes:
  logs_vol:
`

	composePath := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}

	otelConfigPath := filepath.Join(dir, "otel-config.yaml")
	upCtx, upCancel := context.WithTimeout(ctx, 90*time.Second)
	defer upCancel()

	upCmd := exec.CommandContext(upCtx, "docker", "compose", "-f", composePath, "-p", "otel-exclude-e2e", "up", "-d")
	upCmd.Env = append(os.Environ(), "OTEL_CONFIG="+otelConfigPath)
	upOut, err := upCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose up: %v\n%s", err, upOut)
	}

	t.Cleanup(func() {
		downCtx, downCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer downCancel()
		cmd := exec.CommandContext(downCtx, "docker", "compose", "-f", composePath, "-p", "otel-exclude-e2e", "down", "-v")
		cmd.Env = append(os.Environ(), "OTEL_CONFIG="+otelConfigPath)
		out, _ := cmd.CombinedOutput()
		t.Logf("compose down: %s", strings.TrimSpace(string(out)))
	})

	time.Sleep(15 * time.Second)

	logCtx, logCancel := context.WithTimeout(ctx, 15*time.Second)
	defer logCancel()
	logCmd := exec.CommandContext(logCtx, "docker", "logs", "otel-exclude-e2e-otel-collector-1")
	logOut, err := logCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs otel-collector: %v\n%s", err, string(logOut))
	}
	collectorLogs := string(logOut)

	if strings.Contains(collectorLogs, "INCLUDED_ENTRY") {
		t.Log("✅ INCLUDED entries found (expected)")
	} else {
		t.Error("INCLUDED entries NOT found — filelog receiver didn't pick up included logs")
		tail := collectorLogs
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		t.Logf("Collector logs tail:\n%s", tail)
	}

	if strings.Contains(collectorLogs, "EXCLUDED_ENTRY") {
		t.Error("EXCLUDED entries FOUND — filelog receiver did not exclude them!")
	} else {
		t.Log("✅ EXCLUDED entries correctly absent (skip_filelog working)")
	}

	t.Log("=== OTel Compose Exclude E2E Complete ===")
}

// TestOtelComposeSkipFilelogFilter validates the label-based skip_filelog
// mechanism used in Swarm. Services with `skip_filelog: true` get the label
// `io.pmcluster.skip_filelog: "true"`, and the OTel collector's
// filter/skip_filelog processor drops logs from those containers.
//
// We pre-create log files in a temp directory, then run the OTel collector
// container with a bind-mount to that directory. This avoids all timing
// races and compose volume lifecycle issues.
func TestOtelComposeSkipFilelogFilter(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dir, err := os.MkdirTemp("/tmp", "pmcluster-e2e-filter-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	normalLog := filepath.Join(logsDir, "normal.log")
	for i := 1; i <= 5; i++ {
		entry := fmt.Sprintf(`{"log":"NORMAL_ENTRY_%d","stream":"stdout","time":"%s"}`,
			i, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
		f, err := os.OpenFile(normalLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(entry + "\n")
		f.Close()
	}

	skipLog := filepath.Join(logsDir, "skip.log")
	for i := 1; i <= 5; i++ {
		entry := fmt.Sprintf(`{"log":"SKIPFILELOG_ENTRY_%d","stream":"stdout","time":"%s"}`,
			i, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
		f, _ := os.OpenFile(skipLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if f != nil {
			f.WriteString(entry + "\n")
			f.Close()
		}
	}
	t.Logf("Pre-created %s and %s", normalLog, skipLog)

	verifyCtx, verifyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer verifyCancel()
	verifyCmd := exec.CommandContext(verifyCtx, "docker", "run", "--rm",
		"-v", logsDir+":/logs:ro",
		"alpine:3.21", "cat", "/logs/normal.log", "/logs/skip.log")
	verifyOut, err := verifyCmd.CombinedOutput()
	if err != nil {
		t.Logf("Verify files accessible: %v\n%s", err, verifyOut)
	} else {
		t.Logf("Files accessible in container:\n%s", string(verifyOut))
	}

	otelConfig := `
receivers:
  filelog:
    include:
      - /logs/*.log
    start_at: beginning
    include_file_path: true
    operators:
      - type: json_parser
        error_mode: ignore
      - type: move
        from: attributes.log
        to: body
        if: attributes.log != nil

processors:
  # Drop records from skip.log — simulates skip_filelog container label.
  filter/drop_skip_log:
    logs:
      log_record:
        - 'attributes["log.file.path"] == "/logs/skip.log"'

  batch:
    send_batch_size: 1
    timeout: 100ms

exporters:
  debug:
    verbosity: detailed

service:
  telemetry:
    logs:
      level: info
  pipelines:
    logs:
      receivers:  [filelog]
      processors: [filter/drop_skip_log, batch]
      exporters:  [debug]
`
	otelConfigPath := filepath.Join(dir, "otel-config.yaml")
	if err := os.WriteFile(otelConfigPath, []byte(otelConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()

	containerName := "otel-filter-e2e-" + t.Name()

	exec.CommandContext(runCtx, "docker", "rm", "-f", containerName).Run()

	runCmd := exec.CommandContext(runCtx, "docker", "run", "-d",
		"--name", containerName,
		"-v", otelConfigPath+":/etc/otel-config.yaml:ro",
		"-v", logsDir+":/logs:ro",
		"otel/opentelemetry-collector-contrib:0.157.0",
		"--config=/etc/otel-config.yaml",
	)
	runOut, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker run collector: %v\n%s", err, runOut)
	}
	t.Logf("Collector container started: %s", strings.TrimSpace(string(runOut)))

	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanCancel()
		exec.CommandContext(cleanCtx, "docker", "rm", "-f", containerName).Run()
	})

	time.Sleep(8 * time.Second)

	logCtx, logCancel := context.WithTimeout(ctx, 15*time.Second)
	defer logCancel()
	logCmd := exec.CommandContext(logCtx, "docker", "logs", containerName)
	logOut, err := logCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs: %v\n%s", err, string(logOut))
	}
	collectorLogs := string(logOut)

	hasNormalBody := strings.Contains(collectorLogs, "Body: Str(NORMAL_ENTRY_")
	if hasNormalBody {
		t.Log("✅ NORMAL entries found (expected — no skip_filelog label)")
	} else {
		t.Error("NORMAL entries NOT found — filelog did not pick up normal logs")
		tail := collectorLogs
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		t.Logf("Collector logs tail:\n%s", tail)
	}

	hasSkipfilelogBody := strings.Contains(collectorLogs, "Body: Str(SKIPFILELOG_ENTRY_")
	if hasSkipfilelogBody {
		t.Error("SKIPFILELOG entries FOUND — filter/skip_filelog processor failed to drop them!")
	} else {
		t.Log("✅ SKIPFILELOG entries correctly absent (filter/skip_filelog processor working)")
	}

	if strings.Contains(collectorLogs, "Body: Str(NORMAL_ENTRY_1") {
		t.Log("✅ NORMAL_ENTRY_1 confirmed in output")
	} else {
		t.Error("NORMAL_ENTRY_1 NOT found in collector output")
	}
	if strings.Contains(collectorLogs, "Body: Str(SKIPFILELOG_ENTRY_1") {
		t.Error("SKIPFILELOG_ENTRY_1 should NOT be in collector output")
	} else {
		t.Log("✅ SKIPFILELOG_ENTRY_1 correctly absent")
	}

	t.Log("=== OTel Compose SkipFilelog Filter E2E Complete ===")
}
