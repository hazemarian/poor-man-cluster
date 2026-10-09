package ui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// TestValidate_RejectsDefaultSessionSecret asserts the forgeable built-in
// default is refused for a login-enabled console (FIX 2).
func TestValidate_RejectsDefaultSessionSecret(t *testing.T) {
	cfg := FromEnv()
	cfg.DataDir = t.TempDir()
	cfg.SessionSecret = []byte(DefaultSessionSecret)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected Validate to reject the built-in default session secret")
	}
	if !strings.Contains(err.Error(), "built-in default") {
		t.Errorf("error should mention the built-in default: %v", err)
	}
}

// TestValidate_AcceptsRandomSecret asserts a random 32-byte secret passes
// (FIX 2).
func TestValidate_AcceptsRandomSecret(t *testing.T) {
	cfg := FromEnv()
	cfg.DataDir = t.TempDir()
	cfg.SessionSecret = []byte("0123456789abcdef0123456789abcdef")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected a 32-byte secret to validate, got %v", err)
	}
}

// TestValidate_LoginDisabledSkipsSecretChecks asserts the default (and even an
// empty) secret is irrelevant when login is disabled — the secret is unused
// behind the Traefik gate (FIX 2).
func TestValidate_LoginDisabledSkipsSecretChecks(t *testing.T) {
	cfg := FromEnv()
	cfg.DataDir = t.TempDir()
	cfg.LoginDisabled = true
	cfg.SessionSecret = []byte(DefaultSessionSecret)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("login-disabled console must ignore the default secret, got %v", err)
	}
	cfg.SessionSecret = nil
	if err := cfg.Validate(); err != nil {
		t.Fatalf("login-disabled console must ignore an empty secret, got %v", err)
	}
}

// TestSessionSecretFor_GeneratesStableNonDefault asserts the auto-provisioned
// secret is non-default, persisted, and stable across reloads (FIX 2).
func TestSessionSecretFor_GeneratesStableNonDefault(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pmcluster-ui.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	first, err := sessionSecretFor(st, ctx)
	if err != nil {
		t.Fatalf("sessionSecretFor: %v", err)
	}
	if string(first) == DefaultSessionSecret {
		t.Fatalf("generated secret equals the built-in default %q", DefaultSessionSecret)
	}
	if len(first) < 32 {
		t.Fatalf("generated secret too short: %d bytes", len(first))
	}

	// Persisted in the store.
	persisted, err := st.GetSetting(ctx, store.KeySessionSecret)
	if err != nil {
		t.Fatalf("secret not persisted: %v", err)
	}
	if persisted != string(first) {
		t.Fatalf("persisted secret = %q, want %q", persisted, first)
	}

	// Stable across a "reload": a fresh call returns the same secret.
	second, err := sessionSecretFor(st, ctx)
	if err != nil {
		t.Fatalf("second sessionSecretFor: %v", err)
	}
	if string(second) != string(first) {
		t.Fatalf("secret not stable across reloads: %q != %q", second, first)
	}
}
