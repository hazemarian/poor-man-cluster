package store

import (
	"context"
	"strings"
	"testing"
)

// TestSetRenderedAndList verifies the rendered snapshot lives on the configs
// row (same table) and is listed only when present.
func TestSetRenderedAndList(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if _, err := st.CreateConfig(ctx, "cluster", "", "traefik-dynamic", "template", "tls:\n  certificates: []", "v0.2.39"); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if _, err := st.CreateConfig(ctx, "cluster", "", "infra-stack", "template", "version: \"3.9\"", "v0.2.39"); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	// Missing config → ErrConfigNotFound.
	if err := st.SetRendered(ctx, "backup-stack", "x"); err != ErrConfigNotFound {
		t.Fatalf("SetRendered(missing) = %v, want ErrConfigNotFound", err)
	}

	// No renders yet.
	list, err := st.ListRenderedConfigs(ctx)
	if err != nil {
		t.Fatalf("ListRenderedConfigs: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("ListRenderedConfigs before renders = %d rows, want 0", len(list))
	}

	if err := st.SetRendered(ctx, "infra-stack", "version: \"3.9\"\nservices: {}"); err != nil {
		t.Fatalf("SetRendered(infra-stack): %v", err)
	}
	if err := st.SetRendered(ctx, "traefik-dynamic", "tls:\n  certificates: []\n"); err != nil {
		t.Fatalf("SetRendered(traefik-dynamic): %v", err)
	}

	row, err := st.GetConfig(ctx, "infra-stack")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if row.RenderedContent != "version: \"3.9\"\nservices: {}" || row.RenderedAt == 0 {
		t.Errorf("GetConfig rendered fields wrong: content=%q at=%d", row.RenderedContent, row.RenderedAt)
	}
	if row.RenderedHash != ConfigHash(row.RenderedContent) {
		t.Errorf("GetConfig rendered_hash = %q, want %q", row.RenderedHash, ConfigHash(row.RenderedContent))
	}

	list, err = st.ListRenderedConfigs(ctx)
	if err != nil {
		t.Fatalf("ListRenderedConfigs: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListRenderedConfigs = %d rows, want 2", len(list))
	}
	if list[0].Name != "infra-stack" || list[1].Name != "traefik-dynamic" {
		t.Errorf("ListRenderedConfigs order wrong: %s, %s", list[0].Name, list[1].Name)
	}
	if !strings.Contains(list[1].RenderedContent, "certificates") {
		t.Errorf("ListRenderedConfigs content missing: %q", list[1].RenderedContent)
	}

	// Re-render overwrites content and bumps rendered_at.
	if err := st.SetRendered(ctx, "infra-stack", "version: \"3.9\"\nservices: {\n  new: true\n}"); err != nil {
		t.Fatalf("SetRendered(infra-stack) again: %v", err)
	}
	row, err = st.GetConfig(ctx, "infra-stack")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if !strings.Contains(row.RenderedContent, "new: true") {
		t.Errorf("SetRendered did not overwrite content: %q", row.RenderedContent)
	}
}
