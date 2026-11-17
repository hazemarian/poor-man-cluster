package store

import (
	"context"
	"testing"
)

// openStackErrStore is the package's throwaway store with one stack row to hang
// error history on.
func openStackErrStore(t *testing.T) *Store {
	t.Helper()
	st := openTestStore(t)
	if _, err := st.DB().ExecContext(context.Background(),
		`INSERT INTO stacks (name, current_revision, created_at, updated_at) VALUES ('demo', 100, 0, 0)`); err != nil {
		t.Fatalf("seed stack: %v", err)
	}
	return st
}

// TestRecordStackError_CollapsesIdenticalRetries is the regression test for the wall of red the console
// showed: a crash-looping service fails on every reconcile pass, and each failure used to occupy its own
// slot in a 20-entry history. One broken deploy is ONE entry, with the attempt count saying how often.
func TestRecordStackError_CollapsesIdenticalRetries(t *testing.T) {
	ctx := context.Background()
	st := openStackErrStore(t)

	const msg = "Deploying stack to the swarm: timed out after 2m0s waiting for depends_on level [db, web]"
	for i := 0; i < 7; i++ {
		if _, err := st.RecordStackError(ctx, "demo", 100, msg); err != nil {
			t.Fatalf("record attempt %d: %v", i, err)
		}
	}

	entries, err := st.ListStackErrors(ctx, "demo", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (seven identical retries of one broken deploy): %+v", len(entries), entries)
	}
	if entries[0].Count != 7 {
		t.Errorf("count = %d, want 7", entries[0].Count)
	}
	if entries[0].Error != msg || entries[0].Revision != 100 {
		t.Errorf("entry lost its identity: %+v", entries[0])
	}
}

// TestRecordStackError_KeepsDistinctFailuresSeparate — collapsing must not swallow real history: a
// different message, or a different revision, is a different outcome.
func TestRecordStackError_KeepsDistinctFailuresSeparate(t *testing.T) {
	ctx := context.Background()
	st := openStackErrStore(t)

	for _, tc := range []struct {
		rev int64
		msg string
	}{
		{100, "first failure"},
		{100, "second failure"}, // same revision, different cause
		{101, "second failure"}, // same cause, different revision
		{100, "first failure"},  // back to the first cause: NOT a retry of the newest entry
	} {
		if _, err := st.RecordStackError(ctx, "demo", tc.rev, tc.msg); err != nil {
			t.Fatalf("record %+v: %v", tc, err)
		}
	}

	entries, err := st.ListStackErrors(ctx, "demo", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4 — each is a distinct outcome: %+v", len(entries), entries)
	}
	if entries[0].Count != 1 || entries[0].Revision != 100 || entries[0].Error != "first failure" {
		t.Errorf("newest entry = %+v, want the last recorded outcome", entries[0])
	}
}

// TestRecordStackError_CleanApplyWritesNothing keeps the existing contract: success is not history.
func TestRecordStackError_CleanApplyWritesNothing(t *testing.T) {
	ctx := context.Background()
	st := openStackErrStore(t)
	if _, err := st.RecordStackError(ctx, "demo", 100, ""); err != nil {
		t.Fatalf("record clean: %v", err)
	}
	entries, err := st.ListStackErrors(ctx, "demo", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(entries))
	}
}
