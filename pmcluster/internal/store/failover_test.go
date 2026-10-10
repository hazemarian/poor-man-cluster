package store

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"testing"
)

// openFailoverStore opens a fresh migrated store for the stack-failover
// marker tests (t.TempDir keeps every case isolated).
func openFailoverStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestStackFailover_SetGetRoundTrip covers the upsert + read path: a fresh
// marker lands unacknowledged, Get returns every field verbatim, and a second
// Set for the same stack updates in place (no duplicate row).
func TestStackFailover_SetGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openFailoverStore(t)

	first := StackFailover{StackName: "demo", FromNode: "node-a", ToNode: "node-b", At: 1700000001}
	if err := s.SetStackFailover(ctx, first); err != nil {
		t.Fatalf("SetStackFailover: %v", err)
	}
	got, err := s.StackFailover(ctx, "demo")
	if err != nil {
		t.Fatalf("StackFailover: %v", err)
	}
	if got.StackName != "demo" || got.FromNode != "node-a" || got.ToNode != "node-b" || got.At != 1700000001 {
		t.Errorf("marker = %+v, want demo node-a→node-b at 1700000001", got)
	}
	if got.Acked {
		t.Error("fresh automatic failover must start unacknowledged")
	}

	// Upsert: same stack, new nodes/timestamp — the row is updated, not
	// duplicated, and the acked flag follows the new value.
	second := StackFailover{StackName: "demo", FromNode: "node-b", ToNode: "node-c", At: 1700000099, Acked: true}
	if err := s.SetStackFailover(ctx, second); err != nil {
		t.Fatalf("SetStackFailover (update): %v", err)
	}
	got, err = s.StackFailover(ctx, "demo")
	if err != nil {
		t.Fatalf("StackFailover (after update): %v", err)
	}
	if got.FromNode != "node-b" || got.ToNode != "node-c" || got.At != 1700000099 || !got.Acked {
		t.Errorf("marker after upsert = %+v, want node-b→node-c at 1700000099 acked", got)
	}
	rows, err := s.ListStackFailovers(ctx)
	if err != nil {
		t.Fatalf("ListStackFailovers: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("ListStackFailovers = %d rows, want 1 (upsert must not duplicate)", len(rows))
	}
}

// TestStackFailover_Ack verifies AckStackFailover flips acked→1 and that a
// later (re-)Set of a fresh marker starts unacknowledged again.
func TestStackFailover_Ack(t *testing.T) {
	ctx := context.Background()
	s := openFailoverStore(t)

	if err := s.SetStackFailover(ctx, StackFailover{StackName: "demo", FromNode: "node-a", ToNode: "node-b", At: 1}); err != nil {
		t.Fatalf("SetStackFailover: %v", err)
	}
	if err := s.AckStackFailover(ctx, "demo"); err != nil {
		t.Fatalf("AckStackFailover: %v", err)
	}
	got, err := s.StackFailover(ctx, "demo")
	if err != nil {
		t.Fatalf("StackFailover: %v", err)
	}
	if !got.Acked {
		t.Error("marker not acknowledged after AckStackFailover")
	}

	// A new automatic failover resets the flag: the upsert writes acked=0.
	if err := s.SetStackFailover(ctx, StackFailover{StackName: "demo", FromNode: "node-c", ToNode: "node-a", At: 2}); err != nil {
		t.Fatalf("SetStackFailover (re-failover): %v", err)
	}
	got, err = s.StackFailover(ctx, "demo")
	if err != nil {
		t.Fatalf("StackFailover (re-failover): %v", err)
	}
	if got.Acked {
		t.Error("a fresh failover marker must start unacknowledged")
	}
}

// TestStackFailover_Clear verifies ClearStackFailover deletes the row and
// reports ErrNotFound when there is nothing to delete.
func TestStackFailover_Clear(t *testing.T) {
	ctx := context.Background()
	s := openFailoverStore(t)

	if err := s.SetStackFailover(ctx, StackFailover{StackName: "demo", FromNode: "node-a", ToNode: "node-b", At: 1}); err != nil {
		t.Fatalf("SetStackFailover: %v", err)
	}
	if err := s.ClearStackFailover(ctx, "demo"); err != nil {
		t.Fatalf("ClearStackFailover: %v", err)
	}
	if _, err := s.StackFailover(ctx, "demo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("StackFailover after clear = %v, want ErrNotFound", err)
	}
	if err := s.ClearStackFailover(ctx, "demo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second ClearStackFailover = %v, want ErrNotFound", err)
	}
}

// TestStackFailover_NotFoundSentinels pins the absence contract: every read /
// update on a stack with no marker answers the package's own ErrNotFound
// sentinel (errors.Is — callers match on it, not on a string).
func TestStackFailover_NotFoundSentinels(t *testing.T) {
	ctx := context.Background()
	s := openFailoverStore(t)

	if _, err := s.StackFailover(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("StackFailover(ghost) = %v, want ErrNotFound", err)
	}
	if err := s.AckStackFailover(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("AckStackFailover(ghost) = %v, want ErrNotFound", err)
	}
	if err := s.ClearStackFailover(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ClearStackFailover(ghost) = %v, want ErrNotFound", err)
	}
}

// TestStackFailover_List covers the multi-marker read: ordered by stack name,
// acked decoded to bool, empty store → empty (nil) slice and no error.
func TestStackFailover_List(t *testing.T) {
	ctx := context.Background()
	s := openFailoverStore(t)

	empty, err := s.ListStackFailovers(ctx)
	if err != nil {
		t.Fatalf("ListStackFailovers (empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListStackFailovers (empty) = %d rows, want 0", len(empty))
	}

	seed := []StackFailover{
		{StackName: "zeta", FromNode: "node-a", ToNode: "node-b", At: 3},
		{StackName: "alpha", FromNode: "node-b", ToNode: "node-a", At: 1, Acked: true},
		{StackName: "mid", FromNode: "node-a", ToNode: "node-c", At: 2},
	}
	for _, f := range seed {
		if err := s.SetStackFailover(ctx, f); err != nil {
			t.Fatalf("SetStackFailover(%s): %v", f.StackName, err)
		}
	}

	got, err := s.ListStackFailovers(ctx)
	if err != nil {
		t.Fatalf("ListStackFailovers: %v", err)
	}
	if len(got) != len(seed) {
		t.Fatalf("ListStackFailovers = %d rows, want %d", len(got), len(seed))
	}
	names := make([]string, 0, len(got))
	for _, f := range got {
		names = append(names, f.StackName)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("ListStackFailovers order = %v, want sorted by stack name", names)
	}
	acked := map[string]bool{}
	for _, f := range got {
		acked[f.StackName] = f.Acked
	}
	if !acked["alpha"] || acked["mid"] || acked["zeta"] {
		t.Errorf("acked flags = %v, want only alpha acked", acked)
	}
	if got[0].StackName != "alpha" {
		t.Errorf("first row = %q, want \"alpha\" (ORDER BY stack_name)", got[0].StackName)
	}
}
