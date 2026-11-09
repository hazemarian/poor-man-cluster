package store

import (
	"context"
	"errors"
	"testing"
)

func TestGitHubApp_UpsertAndClear(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.GitHubApp(ctx); !errors.Is(err, ErrGitHubAppNotConfigured) {
		t.Fatalf("GitHubApp on an empty store = %v, want ErrGitHubAppNotConfigured", err)
	}

	row := &GitHubApp{
		AppID:                42,
		InstallationID:       7,
		PrivateKeyCiphertext: []byte("ciphertext"),
		ProbeRepo:            "acme/app",
		ProbePackage:         "acme/app:latest",
	}
	if err := s.SetGitHubApp(ctx, row); err != nil {
		t.Fatalf("SetGitHubApp: %v", err)
	}
	got, err := s.GitHubApp(ctx)
	if err != nil {
		t.Fatalf("GitHubApp: %v", err)
	}
	if got.AppID != 42 || got.InstallationID != 7 || got.ProbeRepo != "acme/app" {
		t.Fatalf("row = %+v", got)
	}
	if got.CreatedAt == 0 || got.UpdatedAt == 0 {
		t.Errorf("timestamps not set: %+v", got)
	}

	// Upsert keeps one row and updates it.
	row.InstallationID = 8
	if err := s.SetGitHubApp(ctx, row); err != nil {
		t.Fatalf("second SetGitHubApp: %v", err)
	}
	got, _ = s.GitHubApp(ctx)
	if got.InstallationID != 8 {
		t.Errorf("installation id = %d, want 8", got.InstallationID)
	}

	if err := s.DeleteGitHubApp(ctx); err != nil {
		t.Fatalf("DeleteGitHubApp: %v", err)
	}
	if _, err := s.GitHubApp(ctx); !errors.Is(err, ErrGitHubAppNotConfigured) {
		t.Errorf("after delete = %v, want ErrGitHubAppNotConfigured", err)
	}
	// Clearing twice is a no-op, not an error.
	if err := s.DeleteGitHubApp(ctx); err != nil {
		t.Errorf("second DeleteGitHubApp: %v", err)
	}
}

func TestGitHubBrokerSecrets_CRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	sec := &GitHubBrokerSecret{
		Repo:             "acme/app",
		SecretCiphertext: []byte("ciphertext"),
		Description:      "deploy workflow",
	}
	if err := s.CreateGitHubBrokerSecret(ctx, sec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.CreateGitHubBrokerSecret(ctx, sec); !errors.Is(err, ErrGitHubBrokerSecretExists) {
		t.Fatalf("duplicate create = %v, want ErrGitHubBrokerSecretExists", err)
	}

	got, err := s.GitHubBrokerSecret(ctx, "acme/app")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Description != "deploy workflow" || string(got.SecretCiphertext) != "ciphertext" {
		t.Fatalf("row = %+v", got)
	}
	if got.LastUsedAt != 0 {
		t.Errorf("LastUsedAt = %d, want 0 before first use", got.LastUsedAt)
	}

	if err := s.MarkGitHubBrokerSecretUsed(ctx, "acme/app"); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	got, _ = s.GitHubBrokerSecret(ctx, "acme/app")
	if got.LastUsedAt == 0 {
		t.Error("MarkUsed did not stamp last_used_at")
	}

	sec.SecretCiphertext = []byte("rotated")
	if err := s.UpdateGitHubBrokerSecret(ctx, sec); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = s.GitHubBrokerSecret(ctx, "acme/app")
	if string(got.SecretCiphertext) != "rotated" {
		t.Errorf("rotation did not land")
	}

	second := &GitHubBrokerSecret{Repo: "acme/other", SecretCiphertext: []byte("x")}
	if err := s.CreateGitHubBrokerSecret(ctx, second); err != nil {
		t.Fatalf("Create second: %v", err)
	}
	list, err := s.ListGitHubBrokerSecrets(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if list[0].Repo != "acme/app" || list[1].Repo != "acme/other" {
		t.Errorf("list order = %v, want repo order", []string{list[0].Repo, list[1].Repo})
	}

	if err := s.DeleteGitHubBrokerSecret(ctx, "acme/app"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.DeleteGitHubBrokerSecret(ctx, "acme/app"); !errors.Is(err, ErrGitHubBrokerSecretNotFound) {
		t.Errorf("second delete = %v, want ErrGitHubBrokerSecretNotFound", err)
	}
	if _, err := s.GitHubBrokerSecret(ctx, "acme/app"); !errors.Is(err, ErrGitHubBrokerSecretNotFound) {
		t.Errorf("read after delete = %v, want ErrGitHubBrokerSecretNotFound", err)
	}
}
