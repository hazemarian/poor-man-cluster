package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSeedSettingOnce_SeedsWhenAbsent(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	if err := s.SeedSettingOnce(ctx, KeyToken, "pmc_abc"); err != nil {
		t.Fatalf("SeedSettingOnce: %v", err)
	}
	got, err := s.GetSetting(ctx, KeyToken)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if got != "pmc_abc" {
		t.Errorf("value = %q, want %q", got, "pmc_abc")
	}
}

// TestSeedSettingOnce_DoesNotOverwriteExisting verifies the "store once"
// contract: once a key exists — including an operator's explicit clear from
// Settings (SetSetting(key, "")) — a subsequent seed must not overwrite it.
func TestSeedSettingOnce_DoesNotOverwriteExisting(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	if err := s.SeedSettingOnce(ctx, KeyToken, "first"); err != nil {
		t.Fatalf("seed first: %v", err)
	}
	if err := s.SeedSettingOnce(ctx, KeyToken, "second"); err != nil {
		t.Fatalf("seed second: %v", err)
	}

	got, err := s.GetSetting(ctx, KeyToken)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if got != "first" {
		t.Errorf("value = %q, want %q (must be stored once, not reseeded)", got, "first")
	}

	// An explicit clear from Settings must also not be reseeded.
	if err := s.SetSetting(ctx, KeyToken, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := s.SeedSettingOnce(ctx, KeyToken, "third"); err != nil {
		t.Fatalf("seed after clear: %v", err)
	}
	got, err = s.GetSetting(ctx, KeyToken)
	if err != nil {
		t.Fatalf("GetSetting after clear: %v", err)
	}
	if got != "" {
		t.Errorf("value after clear = %q, want empty (explicit clear must be respected)", got)
	}
}
