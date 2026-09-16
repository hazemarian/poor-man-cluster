package store

import (
	"context"
	"errors"
	"testing"
)

func TestRenderedConfigsCRUD(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if _, err := st.GetRenderedConfig(ctx, "traefik-dynamic"); !errors.Is(err, ErrRenderedConfigNotFound) {
		t.Fatalf("GetRenderedConfig missing = %v, want ErrRenderedConfigNotFound", err)
	}

	if err := st.PutRenderedConfig(ctx, "traefik-dynamic", "tls:\n  certificates: []\n"); err != nil {
		t.Fatalf("PutRenderedConfig: %v", err)
	}
	row, err := st.GetRenderedConfig(ctx, "traefik-dynamic")
	if err != nil {
		t.Fatalf("GetRenderedConfig: %v", err)
	}
	if row.Content != "tls:\n  certificates: []\n" {
		t.Errorf("content = %q, want the stored YAML", row.Content)
	}
	if row.Hash != ConfigHash(row.Content) {
		t.Errorf("hash = %q, want %q", row.Hash, ConfigHash(row.Content))
	}
	createdAt := row.CreatedAt

	// Upsert keeps created_at, moves content/hash/updated_at.
	if err := st.PutRenderedConfig(ctx, "traefik-dynamic", "tls:\n  certificates:\n    - certFile: /run/secrets/cert_v041\n"); err != nil {
		t.Fatalf("PutRenderedConfig (upsert): %v", err)
	}
	row, err = st.GetRenderedConfig(ctx, "traefik-dynamic")
	if err != nil {
		t.Fatalf("GetRenderedConfig (after upsert): %v", err)
	}
	if row.CreatedAt != createdAt {
		t.Errorf("created_at changed on upsert: %d → %d", createdAt, row.CreatedAt)
	}
	if row.UpdatedAt < row.CreatedAt {
		t.Errorf("updated_at (%d) moved before created_at (%d)", row.UpdatedAt, row.CreatedAt)
	}

	if err := st.PutRenderedConfig(ctx, "infra-stack", "version: \"3.9\"\nservices: {}\n"); err != nil {
		t.Fatalf("PutRenderedConfig (second): %v", err)
	}

	rows, err := st.ListRenderedConfigs(ctx)
	if err != nil {
		t.Fatalf("ListRenderedConfigs: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListRenderedConfigs len = %d, want 2", len(rows))
	}
	if rows[0].Name != "infra-stack" || rows[1].Name != "traefik-dynamic" {
		t.Errorf("list not ordered by name: %q, %q", rows[0].Name, rows[1].Name)
	}
}
