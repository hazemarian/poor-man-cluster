package api

import (
	"context"
	"testing"
)

// TestLastBackupJSON_NoneRecorded returns nil when the stack has no
// backup history yet.
func TestLastBackupJSON_NoneRecorded(t *testing.T) {
	st := newTestStore(t)
	got := lastBackupJSON(context.Background(), st, "no-such-stack")
	if got != nil {
		t.Errorf("expected nil for unknown stack, got %v", got)
	}
}

// TestLastBackupJSON_Succeeded returns the most-recent succeeded
// backup with status + timestamps + revision (no error_message).
func TestLastBackupJSON_Succeeded(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.CreateBackup(ctx, "my-stack", 1700000000)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if err := st.FinishBackup(ctx, id, "succeeded", "/var/backups/x.tar.gz", ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	got := lastBackupJSON(ctx, st, "my-stack")
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got["status"] != "succeeded" {
		t.Errorf("status: got %v, want \"succeeded\"", got["status"])
	}
	if got["revision"] != int64(1700000000) {
		t.Errorf("revision: got %v, want 1700000000", got["revision"])
	}
	if _, ok := got["error_message"]; ok {
		t.Errorf("error_message must be omitted on success, got %v", got["error_message"])
	}
	if _, ok := got["finished_at"]; !ok {
		t.Errorf("finished_at must be present on completed backup")
	}
}

// TestLastBackupJSON_Failed surfaces the error message so operators
// can see WHY the last backup failed without an extra round-trip.
func TestLastBackupJSON_Failed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.CreateBackup(ctx, "my-stack", 1700000001)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if err := st.FinishBackup(ctx, id, "failed", "", "offen exec exited 1"); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	got := lastBackupJSON(ctx, st, "my-stack")
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got["status"] != "failed" {
		t.Errorf("status: got %v, want \"failed\"", got["status"])
	}
	if got["error_message"] != "offen exec exited 1" {
		t.Errorf("error_message: got %v", got["error_message"])
	}
}

// TestLastBackupJSON_MostRecentFirst returns only the newest of
// multiple backups for the same stack.
func TestLastBackupJSON_MostRecentFirst(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	old, _ := st.CreateBackup(ctx, "my-stack", 1700000000)
	_ = st.FinishBackup(ctx, old, "succeeded", "", "")

	newer, _ := st.CreateBackup(ctx, "my-stack", 1700000099)
	_ = st.FinishBackup(ctx, newer, "failed", "", "boom")

	got := lastBackupJSON(ctx, st, "my-stack")
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got["status"] != "failed" {
		t.Errorf("expected newest (failed) backup, got status %v", got["status"])
	}
	if got["revision"] != int64(1700000099) {
		t.Errorf("expected newest revision 1700000099, got %v", got["revision"])
	}
}
