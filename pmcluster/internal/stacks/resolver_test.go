package stacks

import (
	"context"
	"strings"
	"testing"
)

func TestStoreConfigResolver(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t.Run("resolves existing config content", func(t *testing.T) {
		_, err := s.CreateConfig(ctx, "service", "", "admin_flag", "env", "true", "v0.2.30")
		if err != nil {
			t.Fatalf("CreateConfig: %v", err)
		}
		r := &StoreConfigResolver{Store: s}
		got, err := r.ResolveConfig(ctx, "admin_flag")
		if err != nil {
			t.Fatalf("ResolveConfig: %v", err)
		}
		if got != "true" {
			t.Errorf("ResolveConfig = %q, want true", got)
		}
	})

	t.Run("missing config returns operator-friendly error", func(t *testing.T) {
		r := &StoreConfigResolver{Store: s}
		_, err := r.ResolveConfig(ctx, "nope")
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
		if _, err := r.ResolveConfig(ctx, "x"); err == nil {
			t.Error("expected error with nil store")
		}
	})
}
