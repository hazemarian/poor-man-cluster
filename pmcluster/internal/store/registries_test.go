package store

import (
	"context"
	"testing"
)

func TestCreateRegistry_HappyPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	r := &Registry{
		Host:               "ghcr.io",
		Username:           "myuser",
		PasswordCiphertext: []byte("ciphertext-bytes"),
	}
	if err := s.CreateRegistry(ctx, r); err != nil {
		t.Fatalf("CreateRegistry: %v", err)
	}

	got, err := s.GetRegistry(ctx, "ghcr.io")
	if err != nil {
		t.Fatalf("GetRegistry: %v", err)
	}
	if got.Host != "ghcr.io" {
		t.Errorf("Host = %q, want 'ghcr.io'", got.Host)
	}
	if got.Username != "myuser" {
		t.Errorf("Username = %q, want 'myuser'", got.Username)
	}
	if string(got.PasswordCiphertext) != "ciphertext-bytes" {
		t.Errorf("PasswordCiphertext = %q, want 'ciphertext-bytes'", got.PasswordCiphertext)
	}
	if got.CreatedAt == 0 {
		t.Error("CreatedAt should be non-zero")
	}
}

func TestCreateRegistry_DuplicateHost(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	r := &Registry{Host: "docker.io", Username: "u1", PasswordCiphertext: []byte("c1")}
	if err := s.CreateRegistry(ctx, r); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	r2 := &Registry{Host: "docker.io", Username: "u2", PasswordCiphertext: []byte("c2")}
	err := s.CreateRegistry(ctx, r2)
	if err != ErrRegistryExists {
		t.Errorf("err = %v, want ErrRegistryExists", err)
	}
}

func TestGetRegistry_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.GetRegistry(ctx, "nonexistent.registry.io")
	if err != ErrRegistryNotFound {
		t.Errorf("err = %v, want ErrRegistryNotFound", err)
	}
}

func TestUpdateRegistry_HappyPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	r := &Registry{Host: "quay.io", Username: "original", PasswordCiphertext: []byte("old-cipher")}
	if err := s.CreateRegistry(ctx, r); err != nil {
		t.Fatalf("CreateRegistry: %v", err)
	}

	updated := &Registry{Host: "quay.io", Username: "rotated", PasswordCiphertext: []byte("new-cipher")}
	if err := s.UpdateRegistry(ctx, updated); err != nil {
		t.Fatalf("UpdateRegistry: %v", err)
	}

	got, err := s.GetRegistry(ctx, "quay.io")
	if err != nil {
		t.Fatalf("GetRegistry: %v", err)
	}
	if got.Username != "rotated" {
		t.Errorf("Username = %q, want 'rotated'", got.Username)
	}
	if string(got.PasswordCiphertext) != "new-cipher" {
		t.Errorf("PasswordCiphertext = %q, want 'new-cipher'", got.PasswordCiphertext)
	}
}

func TestUpdateRegistry_UnknownHost(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	r := &Registry{Host: "unknown.io", Username: "u", PasswordCiphertext: []byte("c")}
	err := s.UpdateRegistry(ctx, r)
	if err != ErrRegistryNotFound {
		t.Errorf("err = %v, want ErrRegistryNotFound", err)
	}
}

func TestListRegistries_AlphabeticalOrder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	hosts := []string{"zoo.io", "alpha.io", "mid.io"}
	for _, h := range hosts {
		if err := s.CreateRegistry(ctx, &Registry{Host: h, Username: "u", PasswordCiphertext: []byte("c")}); err != nil {
			t.Fatalf("CreateRegistry(%s): %v", h, err)
		}
	}

	list, err := s.ListRegistries(ctx)
	if err != nil {
		t.Fatalf("ListRegistries: %v", err)
	}
	if len(list) != len(hosts) {
		t.Fatalf("got %d rows, want %d", len(list), len(hosts))
	}

	want := []string{"alpha.io", "mid.io", "zoo.io"}
	for i, w := range want {
		if list[i].Host != w {
			t.Errorf("list[%d].Host = %q, want %q", i, list[i].Host, w)
		}
	}
}

func TestDeleteRegistry_HappyPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.CreateRegistry(ctx, &Registry{Host: "to-delete.io", Username: "u", PasswordCiphertext: []byte("c")}); err != nil {
		t.Fatalf("CreateRegistry: %v", err)
	}

	if err := s.DeleteRegistry(ctx, "to-delete.io"); err != nil {
		t.Fatalf("DeleteRegistry: %v", err)
	}

	_, err := s.GetRegistry(ctx, "to-delete.io")
	if err != ErrRegistryNotFound {
		t.Errorf("expected ErrRegistryNotFound after delete, got %v", err)
	}
}

func TestDeleteRegistry_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.DeleteRegistry(ctx, "never.io")
	if err != ErrRegistryNotFound {
		t.Errorf("err = %v, want ErrRegistryNotFound", err)
	}
}
