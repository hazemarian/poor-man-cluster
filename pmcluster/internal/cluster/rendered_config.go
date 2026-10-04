package cluster

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// RenderedConfig is one of the two NON-SERVICE platform artifacts whose
// content is rendered from the RenderInput rather than being a stack compose
// template: the OTel collector pipeline config and the Traefik file-provider
// dynamic config. Both flow through the same path — render → EnsureConfig
// (content-aware _v<N> versioning) — so they are handled exactly the same as
// the platform stack composes and the user DSL manifests. This is the L4
// "ONE pipeline" unification: the Traefik and OTel templates are no longer
// special-cased; they are ordinary rendered platform configs.
type RenderedConfig struct {
	// Logical is the base Docker config name (e.g. pmcluster_otel_config).
	// EnsureConfig mints pmcluster_otel_config_v<N> from it.
	Logical string
	// SnapshotKey is the store rendered-config key (e.g. otel-collector-config)
	// used by SetRendered/renderPlatformConfigs for the hash-drift comparison.
	SnapshotKey string
	// Render produces the config bytes from the render state.
	Render func(RenderInput) ([]byte, error)
}

// RenderedConfigs returns the canonical list of non-service platform configs.
// Both are ensured in this order (otel first — the collector must exist before
// the observability stack mounts it; traefik second — the dynamic config
// references the OTel stack name only via Traefik's provider, order is
// cosmetic but kept stable for snapshot consistency).
func RenderedConfigs() []RenderedConfig {
	return []RenderedConfig{
		{Logical: "pmcluster_otel_config", SnapshotKey: "otel-collector-config", Render: RenderOTelCollectorConfig},
		{Logical: "pmcluster_traefik_dynamic", SnapshotKey: "traefik-dynamic", Render: RenderTraefikDynamic},
	}
}

// RenderedConfigOutcome records the result of ensuring one rendered config.
type RenderedConfigOutcome struct {
	Name    string // versioned Docker config name (e.g. pmcluster_otel_config_v3)
	Created bool   // true when a new version was minted
}

// EnsureRenderedConfigs renders and content-versions every non-service
// platform config in one pass. The resulting versioned names are written back
// into render.OTelConfigName / render.TraefikConfigName so subsequent stack
// renders reference the versioned configs. The returned map is keyed by
// Logical name.
func EnsureRenderedConfigs(ctx context.Context, d runtime.Client, render *RenderInput, version string) (map[string]RenderedConfigOutcome, error) {
	outcomes := make(map[string]RenderedConfigOutcome, len(renderedConfigs()))
	for _, rc := range renderedConfigs() {
		data, err := rc.Render(*render)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", rc.Logical, err)
		}
		name, created, err := EnsureConfig(ctx, d, rc.Logical, data, version)
		if err != nil {
			return nil, fmt.Errorf("ensure %s: %w", rc.Logical, err)
		}
		outcomes[rc.Logical] = RenderedConfigOutcome{Name: name, Created: created}
		switch rc.Logical {
		case "pmcluster_otel_config":
			render.OTelConfigName = name
		case "pmcluster_traefik_dynamic":
			render.TraefikConfigName = name
		}
	}
	return outcomes, nil
}

// renderRenderedConfigs renders every non-service platform config WITHOUT
// ensuring it — used by renderPlatformConfigs and RenderClusterConfigs for
// snapshotting and display. The map is keyed by SnapshotKey so the store's
// rendered-config rows keep their historical keys.
func renderRenderedConfigs(render RenderInput) (map[string][]byte, error) {
	out := make(map[string][]byte, len(renderedConfigs()))
	for _, rc := range renderedConfigs() {
		data, err := rc.Render(render)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", rc.Logical, err)
		}
		out[rc.SnapshotKey] = data
	}
	return out, nil
}

// renderedConfigs is a package-level cache so RenderedConfigs() stays
// cheap to call from the hot reconcile path.
func renderedConfigs() []RenderedConfig {
	return []RenderedConfig{
		{Logical: "pmcluster_otel_config", SnapshotKey: "otel-collector-config", Render: RenderOTelCollectorConfig},
		{Logical: "pmcluster_traefik_dynamic", SnapshotKey: "traefik-dynamic", Render: RenderTraefikDynamic},
	}
}
