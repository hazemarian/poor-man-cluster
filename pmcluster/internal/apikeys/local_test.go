package apikeys

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

func TestLocalCreateReturnsPmcTokenAndPersists(t *testing.T) {
	ctx := context.Background()
	svc := NewLocal(openTestStore(t))

	id, token, err := svc.Create(ctx, "alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id <= 0 {
		t.Fatalf("Create id = %d, want > 0", id)
	}
	if !strings.HasPrefix(token, "pmc_") {
		t.Errorf("token = %q, want pmc_ prefix", token)
	}

	keys, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("List = %d keys, want 1", len(keys))
	}
	if keys[0].Name != "alice" || keys[0].ID != id {
		t.Errorf("List = %+v, want alice/%d", keys, id)
	}
}

func TestLocalDeleteEdgeUserProtected(t *testing.T) {
	ctx := context.Background()
	svc := NewLocal(openTestStore(t))

	// The 'edge' user is the operator-console identity; it must never be
	// deletable, even by a valid id.
	id, _, err := svc.Create(ctx, "edge")
	if err != nil {
		t.Fatalf("Create edge: %v", err)
	}

	if err := svc.Delete(ctx, id); !errors.Is(err, ErrEdgeUserProtected) {
		t.Errorf("Delete(edge) = %v, want ErrEdgeUserProtected", err)
	}

	keys, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("edge user was removed: List = %+v, want 1 remaining", keys)
	}
}

func TestLocalDeleteOrdinaryUser(t *testing.T) {
	ctx := context.Background()
	svc := NewLocal(openTestStore(t))

	id, _, err := svc.Create(ctx, "alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Delete(ctx, id); err != nil {
		t.Fatalf("Delete(alice): %v", err)
	}
	keys, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("List after delete = %+v, want empty", keys)
	}
}

func TestLocalListSurfacesLastUsedAt(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	svc := NewLocal(st)

	id, token, err := svc.Create(ctx, "alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Authenticating with the token must bump last_used_at.
	if _, err := st.UserByToken(ctx, token); err != nil {
		t.Fatalf("UserByToken: %v", err)
	}

	keys, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != id {
		t.Fatalf("List = %+v, want single alice/%d", keys, id)
	}
	if keys[0].LastUsedAt == 0 {
		t.Errorf("LastUsedAt = 0 after auth, want non-zero")
	}
}
