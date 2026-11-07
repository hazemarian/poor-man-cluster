package cluster

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// renderedServiceEnv returns one service's env from a rendered stack.
func renderedServiceEnv(t *testing.T, rendered []byte, service string) map[string]string {
	t.Helper()
	var doc struct {
		Services map[string]struct {
			Environment map[string]string `json:"environment"`
			Env         map[string]string `json:"env"`
			Command     []string          `json:"command"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		t.Fatalf("unmarshal rendered stack: %v", err)
	}
	svc, ok := doc.Services[service]
	if !ok {
		t.Fatalf("rendered stack has no %q service", service)
	}
	if svc.Environment != nil {
		return svc.Environment
	}
	return svc.Env
}

// TestRenderedInfraStackDisablesTraefikTracing: Traefik must not produce
// spans. The flag stays explicit — deleting it would read as "never decided"
// and the next person would turn the block back on by copying an example.
func TestRenderedInfraStackDisablesTraefikTracing(t *testing.T) {
	data, err := LoadComposeFile(StackInfra, RenderInput{Domain: "example.com"})
	if err != nil {
		t.Fatalf("LoadComposeFile(infra): %v", err)
	}
	body := string(data)

	if !strings.Contains(body, "--tracing.otlp=false") {
		t.Errorf("rendered infra stack does not disable OTLP tracing:\n%s", body)
	}
	if strings.Contains(body, "--tracing.otlp=true") {
		t.Errorf("rendered infra stack still enables OTLP tracing:\n%s", body)
	}
	// The endpoint/serviceName flags stay so the intent (and the way back) is
	// visible next to the switch.
	for _, want := range []string{
		"--tracing.otlp.grpc.endpoint=observability_otel-collector:4317",
		"--tracing.serviceName=traefik-proxy",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered infra stack dropped %q", want)
		}
	}
}

// TestEdgeStackRendersPlatformLogLevel: the log_level setting reaches the edge
// container as PMCLUSTER_LOG_LEVEL — the same env var the daemon reads — so one
// setting covers both processes.
func TestEdgeStackRendersPlatformLogLevel(t *testing.T) {
	data, err := LoadComposeFile(StackEdge, RenderInput{Domain: "example.com", LogLevel: "warn"})
	if err != nil {
		t.Fatalf("LoadComposeFile(edge): %v", err)
	}
	env := renderedServiceEnv(t, data, "pmcluster-edge")
	if got := env["PMCLUSTER_LOG_LEVEL"]; got != "warn" {
		t.Errorf("edge PMCLUSTER_LOG_LEVEL = %q, want warn", got)
	}
}

// TestEdgeStackLogLevelDefaultsToInfo: an unset setting renders info, not an
// empty value the edge would have to guess at.
func TestEdgeStackLogLevelDefaultsToInfo(t *testing.T) {
	data, err := LoadComposeFile(StackEdge, RenderInput{Domain: "example.com"})
	if err != nil {
		t.Fatalf("LoadComposeFile(edge): %v", err)
	}
	env := renderedServiceEnv(t, data, "pmcluster-edge")
	if got := env["PMCLUSTER_LOG_LEVEL"]; got != "info" {
		t.Errorf("edge PMCLUSTER_LOG_LEVEL = %q, want info (the default)", got)
	}
}
