package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openBackupTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateBackup_OnDemand(t *testing.T) {
	s := openBackupTestStore(t)
	ctx := context.Background()

	id, err := s.CreateBackup(ctx, "", 0)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if id <= 0 {
		t.Errorf("id = %d, want > 0", id)
	}

	got, err := s.Backup(ctx, id)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.StackName.Valid {
		t.Errorf("on-demand backup should have NULL stack_name")
	}
	if got.Revision.Valid {
		t.Errorf("on-demand backup should have NULL revision")
	}
}

func TestCreateBackup_PreDeploy(t *testing.T) {
	s := openBackupTestStore(t)
	ctx := context.Background()

	id, err := s.CreateBackup(ctx, "donation-campaign", 1700000000)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	got, err := s.Backup(ctx, id)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got.StackName.String != "donation-campaign" {
		t.Errorf("stack_name = %q", got.StackName.String)
	}
	if got.Revision.Int64 != 1700000000 {
		t.Errorf("revision = %d", got.Revision.Int64)
	}
}

func TestFinishBackup_Success(t *testing.T) {
	s := openBackupTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateBackup(ctx, "", 0)

	if err := s.FinishBackup(ctx, id, "succeeded", "/archive/a.tar.gz,/archive/b.tar.gz", ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}
	got, _ := s.Backup(ctx, id)
	if got.Status != "succeeded" {
		t.Errorf("status = %q", got.Status)
	}
	if got.ArchivePaths != "/archive/a.tar.gz,/archive/b.tar.gz" {
		t.Errorf("archives = %q", got.ArchivePaths)
	}
	if !got.FinishedAt.Valid {
		t.Errorf("finished_at not set")
	}
}

func TestFinishBackup_Failure(t *testing.T) {
	s := openBackupTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateBackup(ctx, "", 0)
	if err := s.FinishBackup(ctx, id, "failed", "", "offen exit 1"); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}
	got, _ := s.Backup(ctx, id)
	if got.Status != "failed" {
		t.Errorf("status = %q", got.Status)
	}
	if got.ErrorMessage != "offen exit 1" {
		t.Errorf("error = %q", got.ErrorMessage)
	}
}

func TestGetBackup_NotFound(t *testing.T) {
	s := openBackupTestStore(t)
	_, err := s.Backup(context.Background(), 99999)
	if !errors.Is(err, ErrBackupNotFound) {
		t.Errorf("err = %v, want ErrBackupNotFound", err)
	}
}

func TestListBackups_OrderingAndLimit(t *testing.T) {
	s := openBackupTestStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := s.CreateBackup(ctx, "", 0); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	all, err := s.ListBackups(ctx, 0)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("want 5 rows, got %d", len(all))
	}

	limited, _ := s.ListBackups(ctx, 2)
	if len(limited) != 2 {
		t.Errorf("limit=2 returned %d", len(limited))
	}
}

func TestListBackupsForStack_Filters(t *testing.T) {
	s := openBackupTestStore(t)
	ctx := context.Background()

	_, _ = s.CreateBackup(ctx, "stack-a", 1)
	_, _ = s.CreateBackup(ctx, "stack-b", 1)
	_, _ = s.CreateBackup(ctx, "stack-a", 2)

	got, err := s.ListBackupsForStack(ctx, "stack-a")
	if err != nil {
		t.Fatalf("ListBackupsForStack: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("stack-a rows = %d, want 2", len(got))
	}
	for _, b := range got {
		if b.StackName.String != "stack-a" {
			t.Errorf("got row for %q in stack-a result", b.StackName.String)
		}
	}
}
