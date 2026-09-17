package backups

import (
	"context"
	"errors"
	"testing"
)

func TestLocalTriggerSuccessRecordsSucceeded(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	svc := NewLocal(st, func(_ context.Context) ([]string, error) {
		return []string{"/archive/a.tar.gz", "/archive/b.tar.gz"}, nil
	})

	id, paths, err := svc.Trigger(ctx, "mystack", 42)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if id <= 0 {
		t.Fatalf("Trigger id = %d, want > 0", id)
	}
	if len(paths) != 2 || paths[0] != "/archive/a.tar.gz" {
		t.Errorf("paths = %v, want two archives", paths)
	}

	row, err := st.GetBackup(ctx, id)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if row.Status != StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", row.Status)
	}
	if row.ArchivePaths != "/archive/a.tar.gz,/archive/b.tar.gz" {
		t.Errorf("ArchivePaths = %q", row.ArchivePaths)
	}
	if !row.StackName.Valid || row.StackName.String != "mystack" {
		t.Errorf("StackName = %+v, want mystack", row.StackName)
	}
	if !row.Revision.Valid || row.Revision.Int64 != 42 {
		t.Errorf("Revision = %+v, want 42", row.Revision)
	}
	if !row.FinishedAt.Valid {
		t.Error("FinishedAt not set on success")
	}
}

func TestLocalTriggerFailureRecordsFailed(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	boom := errors.New("offen blew up")
	svc := NewLocal(st, func(_ context.Context) ([]string, error) {
		return nil, boom
	})

	_, _, err := svc.Trigger(ctx, "", 0)
	if !errors.Is(err, boom) {
		t.Fatalf("Trigger error = %v, want the injected error", err)
	}

	rows, err := st.ListBackups(ctx, 10)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Status != StatusFailed {
		t.Errorf("Status = %q, want failed", rows[0].Status)
	}
	if rows[0].ErrorMessage != "offen blew up" {
		t.Errorf("ErrorMessage = %q, want offen blew up", rows[0].ErrorMessage)
	}
}

func TestLocalTriggerNilRunNotConfigured(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	svc := NewLocal(st, nil)

	id, _, err := svc.Trigger(ctx, "", 0)
	if !errors.Is(err, ErrTriggerNotConfigured) {
		t.Fatalf("Trigger error = %v, want ErrTriggerNotConfigured", err)
	}
	if id <= 0 {
		t.Fatalf("Trigger id = %d, want > 0 (run still recorded)", id)
	}

	row, err := st.GetBackup(ctx, id)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if row.Status != "pending" {
		t.Errorf("Status = %q, want pending (unfinished)", row.Status)
	}
}

func TestLocalListAndListForStack(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	svc := NewLocal(st, nil)

	if _, err := st.CreateBackup(ctx, "alpha", 1); err != nil {
		t.Fatalf("CreateBackup alpha: %v", err)
	}
	if _, err := st.CreateBackup(ctx, "beta", 1); err != nil {
		t.Fatalf("CreateBackup beta: %v", err)
	}

	all, err := svc.List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List = %d runs, want 2", len(all))
	}

	alpha, err := svc.ListForStack(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListForStack: %v", err)
	}
	if len(alpha) != 1 || alpha[0].StackName != "alpha" {
		t.Errorf("ListForStack(alpha) = %+v, want single alpha run", alpha)
	}
}
