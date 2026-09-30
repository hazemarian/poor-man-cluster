package stacks

import (
	"context"
	"strings"
	"testing"

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
