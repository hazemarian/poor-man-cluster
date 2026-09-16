package cluster

import (
	"context"
	"strings"
	"testing"
)

// TestRenderClusterConfigs verifies the read-only render of all six platform
// configs returns fully-substituted YAML without deploying anything.
func TestRenderClusterConfigs(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)

	out, err := RenderClusterConfigs(context.Background(), deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"})
	if err != nil {
		t.Fatalf("RenderClusterConfigs: %v", err)
	}

	for _, key := range []string{"infra-stack", "observability-stack", "backup-stack", "edge-stack", "otel-collector-config", "traefik-dynamic"} {
		content, ok := out[key]
		if !ok {
			t.Errorf("missing rendered config %q", key)
			continue
		}
		if strings.TrimSpace(content) == "" {
			t.Errorf("rendered config %q is empty", key)
		}
	}

	for _, placeholder := range []string{"[[.", "${DOMAIN}", "__OTEL_CONFIG_NAME__", "__TRAEFIK_CONFIG_NAME__"} {
		if strings.Contains(out["traefik-dynamic"], placeholder) {
			t.Errorf("traefik-dynamic still contains placeholder %q", placeholder)
		}
	}

	// seedUpdateState provisions TLS in cert mode, so the rendered dynamic
	// config must reference the versioned cert/key Swarm secrets.
	if !strings.Contains(out["traefik-dynamic"], "cert_") {
		t.Errorf("traefik-dynamic missing cert secret reference")
	}
}

// TestUpdate_PersistsRenderedConfigs verifies cluster update snapshots the
// rendered platform configs into the database (the console reads them back).
func TestUpdate_PersistsRenderedConfigs(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)

	if _, err := Update(context.Background(), deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	for _, name := range []string{"infra-stack", "observability-stack", "backup-stack", "edge-stack", "otel-collector-config", "traefik-dynamic"} {
		if _, err := deps.Store.GetRenderedConfig(context.Background(), name); err != nil {
			t.Errorf("rendered config %q not persisted after Update: %v", name, err)
		}
	}
}
