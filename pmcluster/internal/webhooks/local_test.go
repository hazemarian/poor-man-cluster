package webhooks

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func newLocal(t *testing.T) (*Local, *store.Store) {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	c, err := credentials.Open(filepath.Join(dir, ".enc_key"))
	if err != nil {
		t.Fatalf("credentials.Open: %v", err)
	}
	return NewLocal(st, c), st
}

func TestLocalCreateListDelete(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLocal(t)

	secret, err := svc.Create(ctx, "github", "ci pushes")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(secret) != 64 {
		t.Errorf("secret length = %d, want 64 hex chars (32 bytes)", len(secret))
	}

	srcs, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("List = %d sources, want 1", len(srcs))
	}
	if srcs[0].Source != "github" || srcs[0].Description != "ci pushes" {
		t.Errorf("List = %+v, want github/ci pushes", srcs)
	}

	if err := svc.Delete(ctx, "github"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	srcs, err = svc.List(ctx)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("List after delete = %+v, want empty", srcs)
	}
	if err := svc.Delete(ctx, "github"); !errors.Is(err, store.ErrWebhookSourceNotFound) {
		t.Errorf("Delete missing: err = %v, want ErrWebhookSourceNotFound", err)
	}
}

func TestLocalSecretDecryptRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLocal(t)

	secret, err := svc.Create(ctx, "gitlab", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.Secret(ctx, "gitlab")
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if string(got) != secret {
		t.Errorf("Secret = %q, want the plaintext returned at create (%q)", got, secret)
	}
}

func TestLocalMarkUsed(t *testing.T) {
	ctx := context.Background()
	svc, st := newLocal(t)

	if _, err := svc.Create(ctx, "github", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.MarkUsed(ctx, "github"); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	row, err := st.GetWebhookSource(ctx, "github")
	if err != nil {
		t.Fatalf("GetWebhookSource: %v", err)
	}
	if !row.LastUsedAt.Valid || row.LastUsedAt.Int64 <= 0 {
		t.Errorf("LastUsedAt = %+v, want a positive timestamp after MarkUsed", row.LastUsedAt)
	}
}
