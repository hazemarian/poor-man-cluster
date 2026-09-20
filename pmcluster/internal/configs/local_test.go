package configs

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestLocalCreateGet(t *testing.T) {
	ctx := context.Background()
	svc := NewLocal(openTestStore(t))

	id, err := svc.Create(ctx, "cluster", "", "otel_config", "template", "key: value", "v1.2.3")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id <= 0 {
		t.Fatalf("Create id = %d, want > 0", id)
	}

	got, err := svc.Get(ctx, "otel_config")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "otel_config" || got.Scope != "cluster" || got.Kind != "template" {
		t.Errorf("Get = %+v, want name/scope/kind preserved", got)
	}
	if got.Content != "key: value" {
		t.Errorf("Content = %q, want key: value", got.Content)
	}
	if got.Hash != store.ConfigHash("key: value") {
		t.Errorf("Hash = %q, want %q", got.Hash, store.ConfigHash("key: value"))
	}
	if got.Version != "v1.2.3" {
		t.Errorf("Version = %q, want v1.2.3", got.Version)
	}
}

func TestLocalUpdateRollback(t *testing.T) {
	ctx := context.Background()
	svc := NewLocal(openTestStore(t))

	if _, err := svc.Create(ctx, "cluster", "", "cfg", "file", "v1", "1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	hash, err := svc.Update(ctx, "cfg", "v2", "1")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if hash != store.ConfigHash("v2") {
		t.Errorf("Update hash = %q, want hash of v2", hash)
	}

	got, err := svc.Get(ctx, "cfg")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Content != "v2" {
		t.Errorf("Content after update = %q, want v2", got.Content)
	}

	vers, err := svc.ListVersions(ctx, "cfg")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(vers) != 1 {
		t.Fatalf("ListVersions = %d entries, want 1 (the pre-update value)", len(vers))
	}
	if vers[0].Hash != store.ConfigHash("v1") {
		t.Errorf("version hash = %q, want hash of v1", vers[0].Hash)
	}

	rolledBack, err := svc.Rollback(ctx, "cfg", vers[0].ID)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolledBack != store.ConfigHash("v1") {
		t.Errorf("Rollback hash = %q, want hash of v1", rolledBack)
	}
	got, err = svc.Get(ctx, "cfg")
	if err != nil {
		t.Fatalf("Get after rollback: %v", err)
	}
	if got.Content != "v1" {
		t.Errorf("Content after rollback = %q, want v1", got.Content)
	}
}

func TestLocalSetRenderedListRendered(t *testing.T) {
	ctx := context.Background()
	svc := NewLocal(openTestStore(t))

	if _, err := svc.Create(ctx, "cluster", "", "tmpl", "template", "hello {{ .Domain }}", "1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.SetRendered(ctx, "tmpl", "hello example.com"); err != nil {
		t.Fatalf("SetRendered: %v", err)
	}

	rows, err := svc.ListRendered(ctx)
	if err != nil {
		t.Fatalf("ListRendered: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListRendered = %d rows, want 1", len(rows))
	}
	if rows[0].Name != "tmpl" || rows[0].Rendered != "hello example.com" {
		t.Errorf("rendered row = %+v", rows[0])
	}
	if rows[0].Content != "" {
		t.Errorf("ListRendered Content = %q, want empty (content withheld)", rows[0].Content)
	}
}

func TestLocalListAndDeleteNotFound(t *testing.T) {
	ctx := context.Background()
	svc := NewLocal(openTestStore(t))

	if _, err := svc.Create(ctx, "service", "a", "one", "file", "1", "1"); err != nil {
		t.Fatalf("Create one: %v", err)
	}
	if _, err := svc.Create(ctx, "cluster", "", "two", "file", "2", "1"); err != nil {
		t.Fatalf("Create two: %v", err)
	}

	svcRows, err := svc.List(ctx, "service", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(svcRows) != 1 || svcRows[0].Name != "one" {
		t.Errorf("List(service) = %+v, want only 'one'", svcRows)
	}

	if err := svc.Delete(ctx, "one"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, "one"); !errors.Is(err, store.ErrConfigNotFound) {
		t.Errorf("Get after delete: err = %v, want ErrConfigNotFound", err)
	}
	if err := svc.Delete(ctx, "one"); !errors.Is(err, store.ErrConfigNotFound) {
		t.Errorf("Delete missing: err = %v, want ErrConfigNotFound", err)
	}
}
