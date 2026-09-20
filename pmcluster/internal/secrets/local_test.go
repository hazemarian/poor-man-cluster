package secrets

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
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

func openTestCipher(t *testing.T) *credentials.Cipher {
	t.Helper()
	c, err := credentials.Open(filepath.Join(t.TempDir(), ".encryption_key"))
	if err != nil {
		t.Fatalf("credentials.Open: %v", err)
	}
	return c
}

func newTestLocal(t *testing.T) (*Local, *store.Store) {
	t.Helper()
	st := openTestStore(t)
	return NewLocal(st, openTestCipher(t)), st
}

func TestLocalCreateGetRevealRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestLocal(t)

	id, err := svc.Create(ctx, "service", "mystack", "DB_PASSWORD", "hunter2")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id <= 0 {
		t.Fatalf("Create id = %d, want > 0", id)
	}

	got, err := svc.Get(ctx, "DB_PASSWORD")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "DB_PASSWORD" || got.Scope != "service" || got.Stack != "mystack" {
		t.Errorf("Get = %+v, want name/scope/stack preserved", got)
	}
	if got.Hash != store.SecretHash("hunter2") {
		t.Errorf("Hash = %q, want %q", got.Hash, store.SecretHash("hunter2"))
	}

	plain, err := svc.Reveal(ctx, "DB_PASSWORD")
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if plain != "hunter2" {
		t.Errorf("Reveal = %q, want hunter2", plain)
	}

	// The ciphertext stored must never equal the plaintext.
	row, err := st.GetSecret(ctx, "DB_PASSWORD")
	if err != nil {
		t.Fatalf("store.GetSecret: %v", err)
	}
	if string(row.Payload) == "hunter2" {
		t.Error("stored payload is plaintext; expected ciphertext")
	}
}

func TestLocalListExcludesPayload(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestLocal(t)

	if _, err := svc.Create(ctx, "service", "a", "S1", "v1"); err != nil {
		t.Fatalf("Create S1: %v", err)
	}
	if _, err := svc.Create(ctx, "cluster", "", "S2", "v2"); err != nil {
		t.Fatalf("Create S2: %v", err)
	}

	all, err := svc.List(ctx, "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List = %d rows, want 2", len(all))
	}

	svcOnly, err := svc.List(ctx, "service", "")
	if err != nil {
		t.Fatalf("List service: %v", err)
	}
	if len(svcOnly) != 1 || svcOnly[0].Name != "S1" {
		t.Errorf("List(service) = %+v, want only S1", svcOnly)
	}
	if svcOnly[0].Hash != store.SecretHash("v1") {
		t.Errorf("S1 hash = %q", svcOnly[0].Hash)
	}
}

func TestLocalUpdateChangesValue(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestLocal(t)

	if _, err := svc.Create(ctx, "service", "", "TOKEN", "old"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Update(ctx, "TOKEN", "new"); err != nil {
		t.Fatalf("Update: %v", err)
	}

	plain, err := svc.Reveal(ctx, "TOKEN")
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if plain != "new" {
		t.Errorf("Reveal after update = %q, want new", plain)
	}
	got, err := svc.Get(ctx, "TOKEN")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Hash != store.SecretHash("new") {
		t.Errorf("Hash after update = %q, want hash of new", got.Hash)
	}
}

func TestLocalDeleteNotFound(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestLocal(t)

	if _, err := svc.Create(ctx, "service", "", "X", "v"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Delete(ctx, "X"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, "X"); !errors.Is(err, store.ErrSecretNotFound) {
		t.Errorf("Get after delete: err = %v, want ErrSecretNotFound", err)
	}
	if err := svc.Delete(ctx, "X"); !errors.Is(err, store.ErrSecretNotFound) {
		t.Errorf("Delete missing: err = %v, want ErrSecretNotFound", err)
	}
}
