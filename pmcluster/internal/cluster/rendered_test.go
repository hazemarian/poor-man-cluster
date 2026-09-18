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

	// No unresolved DSL refs may survive in any rendered config. The
	// config()/secrets() references (formerly __X__ placeholders) must be
	// substituted with real artifact names by the time a stack renders.
	for key, content := range out {
		for _, placeholder := range []string{"[[.", "${DOMAIN}", "config(", "secrets("} {
			if strings.Contains(content, placeholder) {
				t.Errorf("%s still contains unresolved %q", key, placeholder)
			}
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
	ctx := context.Background()

	// seedUpdateState's Up already synced the six cluster template rows into
	// the store. Version v0.3.0 != buildinfo.Version ("dev" in tests), so these
	// rows are not picked up as the render source — renders come from disk —
	// but they give the render snapshots a row to live on (SetRendered skips
	// missing rows).
	names := []string{"infra-stack", "observability-stack", "backup-stack", "edge-stack", "otel-collector-config", "traefik-dynamic"}

	if _, err := Update(ctx, deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	for _, name := range names {
		row, err := deps.Store.GetConfig(ctx, name)
		if err != nil {
			t.Errorf("config %q missing after Update: %v", name, err)
			continue
		}
		if strings.TrimSpace(row.RenderedContent) == "" {
			t.Errorf("rendered config %q not persisted after Update", name)
		}
	}
}
