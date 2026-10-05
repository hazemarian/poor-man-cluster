package stacks

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func TestStoreConfigResolver(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t.Run("resolves own-stack config content", func(t *testing.T) {
		_, err := s.CreateConfig(ctx, "service", "demo", "admin_flag", "env", "true", "v0.2.30")
		if err != nil {
			t.Fatalf("CreateConfig: %v", err)
		}
		r := &StoreConfigResolver{Store: s}
		got, err := r.ResolveConfig(ctx, "demo", "admin_flag")
		if err != nil {
			t.Fatalf("ResolveConfig: %v", err)
		}
		if got != "true" {
			t.Errorf("ResolveConfig = %q, want true", got)
		}
	})

	t.Run("falls back to shared empty-stack config", func(t *testing.T) {
		_, err := s.CreateConfig(ctx, "service", "", "shared_key", "env", "shared", "v0.2.30")
		if err != nil {
			t.Fatalf("CreateConfig: %v", err)
		}
		r := &StoreConfigResolver{Store: s}
		got, err := r.ResolveConfig(ctx, "demo", "shared_key")
		if err != nil {
			t.Fatalf("ResolveConfig shared: %v", err)
		}
		if got != "shared" {
			t.Errorf("ResolveConfig = %q, want shared", got)
		}
	})

	t.Run("never resolves another stack's config", func(t *testing.T) {
		_, err := s.CreateConfig(ctx, "service", "other", "solo_secret", "env", "sensitive", "v0.2.30")
		if err != nil {
			t.Fatalf("CreateConfig: %v", err)
		}
		r := &StoreConfigResolver{Store: s}
		_, err = r.ResolveConfig(ctx, "demo", "solo_secret")
		if err == nil {
			t.Fatal("expected error resolving another stack's config")
		}
		if !strings.Contains(err.Error(), `not found for stack "demo"`) {
			t.Errorf("error should name the stack: %v", err)
		}
		// The owning stack still resolves it.
		got, err := r.ResolveConfig(ctx, "other", "solo_secret")
		if err != nil || got != "sensitive" {
			t.Errorf("owner should resolve own config: got %q err %v", got, err)
		}
	})

	t.Run("missing config returns operator-friendly error", func(t *testing.T) {
		r := &StoreConfigResolver{Store: s}
		_, err := r.ResolveConfig(ctx, "demo", "nope")
		if err == nil {
			t.Fatal("expected error for missing config")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error should mention not found: %v", err)
		}
		if !strings.Contains(err.Error(), "pmcluster config create") {
			t.Errorf("error should hint the create command: %v", err)
		}
	})

	t.Run("nil store errors", func(t *testing.T) {
		r := &StoreConfigResolver{}
		if _, err := r.ResolveConfig(ctx, "demo", "x"); err == nil {
			t.Error("expected error with nil store")
		}
	})
}

// TestStoreConfigResolver_ResolveSetting exercises the settings() env-ref path:
// a manifest value of settings(name) is injected from the cluster_settings
// table, missing keys fail loud with the set command hint, and nil store errors.
func TestStoreConfigResolver_ResolveSetting(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.SetSetting(ctx, "volume_root", "/srv/stack/data"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	r := &StoreConfigResolver{Store: s}

	t.Run("resolves known setting", func(t *testing.T) {
		got, err := r.ResolveSetting(ctx, "demo", "volume_root")
		if err != nil {
			t.Fatalf("ResolveSetting: %v", err)
		}
		if got != "/srv/stack/data" {
			t.Errorf("ResolveSetting = %q, want /srv/stack/data", got)
		}
	})

	t.Run("missing setting errors with hint", func(t *testing.T) {
		_, err := r.ResolveSetting(ctx, "demo", "ghost_key")
		if err == nil {
			t.Fatal("expected error for missing setting")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error should mention not found: %v", err)
		}
		if !strings.Contains(err.Error(), "cluster settings set ghost_key") {
			t.Errorf("error should hint the settings command: %v", err)
		}
	})

	t.Run("nil store errors", func(t *testing.T) {
		r := &StoreConfigResolver{}
		if _, err := r.ResolveSetting(ctx, "demo", "x"); err == nil {
			t.Error("expected error with nil store")
		}
	})
}

// TestGetConfigForStack directly exercises the store scoping rule the
// resolver relies on.
func TestGetConfigForStack(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	must := func(scope, stack, name, content string) {
		t.Helper()
		if _, err := s.CreateConfig(ctx, scope, stack, name, "env", content, "v0.2.30"); err != nil {
			t.Fatalf("CreateConfig(%s,%s,%s): %v", scope, stack, name, err)
		}
	}
	must("service", "demo", "demo_cfg", "demo-val")
	must("service", "other", "other_cfg", "other-val")
	must("service", "", "shared_cfg", "shared-val")

	tests := []struct {
		stack, name string
		wantContent string
		wantErr     bool
	}{
		{"demo", "demo_cfg", "demo-val", false},
		{"demo", "shared_cfg", "shared-val", false},
		{"demo", "other_cfg", "", true}, // other stack's config is NOT leaked
		{"other", "demo_cfg", "", true},
		{"demo", "ghost", "", true},
	}
	for _, tt := range tests {
		row, err := s.GetConfigForStack(ctx, tt.stack, tt.name)
		if tt.wantErr {
			if err == nil {
				t.Errorf("GetConfigForStack(%s,%s): expected error", tt.stack, tt.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("GetConfigForStack(%s,%s): %v", tt.stack, tt.name, err)
			continue
		}
		if row.Content != tt.wantContent {
			t.Errorf("GetConfigForStack(%s,%s) = %q, want %q", tt.stack, tt.name, row.Content, tt.wantContent)
		}
	}

	// Cross-stack check must hold even when the store also holds an
	// unrelated row — ensure foreign rows never surface.
	if _, err := s.GetConfigForStack(ctx, "demo", "other_cfg"); err != store.ErrConfigNotFound {
		t.Errorf("foreign config surfaced: err=%v", err)
	}
}

// TestStoreConfigResolver_ResolveSecretValue exercises the secret(<name>)
// env-ref path: the plaintext is decrypted from the DB ciphertext with the
// cluster encryption key (Docker secrets are write-only — no payload
// readback), cross-stack rows are refused, and a missing Cipher fails loud.
func TestStoreConfigResolver_ResolveSecretValue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	keyDir := t.TempDir()
	c, err := credentials.Open(filepath.Join(keyDir, ".encryption_key"))
	if err != nil {
		t.Fatalf("open cipher: %v", err)
	}
	ct, err := c.Encrypt([]byte("s3cret-value-1"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := s.CreateSecret(ctx, "service", "demo", "t2_pass", ct, store.SecretHash("s3cret-value-1")); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	r := &StoreConfigResolver{Store: s, Cipher: c}

	t.Run("decrypts own-stack secret value", func(t *testing.T) {
		got, err := r.ResolveSecretValue(ctx, "demo", "t2_pass")
		if err != nil {
			t.Fatalf("ResolveSecretValue: %v", err)
		}
		if got != "s3cret-value-1" {
			t.Errorf("ResolveSecretValue = %q, want %q", got, "s3cret-value-1")
		}
	})

	t.Run("cross-stack secret refused", func(t *testing.T) {
		if _, err := r.ResolveSecretValue(ctx, "other-stack", "t2_pass"); err == nil {
			t.Error("expected error for cross-stack secret")
		} else if !strings.Contains(err.Error(), "belongs to stack") {
			t.Errorf("error should mention stack ownership: %v", err)
		}
	})

	t.Run("missing secret errors with hint", func(t *testing.T) {
		_, err := r.ResolveSecretValue(ctx, "demo", "ghost_pass")
		if err == nil {
			t.Fatal("expected error for missing secret")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error should mention not found: %v", err)
		}
	})

	t.Run("nil cipher errors", func(t *testing.T) {
		r := &StoreConfigResolver{Store: s}
		if _, err := r.ResolveSecretValue(ctx, "demo", "t2_pass"); err == nil {
			t.Error("expected error with nil cipher")
		} else if !strings.Contains(err.Error(), "encryption key") {
			t.Errorf("error should mention encryption key: %v", err)
		}
	})

	t.Run("nil store errors", func(t *testing.T) {
		r := &StoreConfigResolver{Cipher: c}
		if _, err := r.ResolveSecretValue(ctx, "demo", "t2_pass"); err == nil {
			t.Error("expected error with nil store")
		}
	})
}
