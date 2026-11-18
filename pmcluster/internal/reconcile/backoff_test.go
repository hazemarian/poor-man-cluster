package reconcile

import (
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// TestBackoffDelay pins the retry ladder. A stack that fails its sync used to be retried on EVERY
// pass — a two-minute health wait each time, measured 58 attempts on one revision — so the ladder is
// both the fix and an operator-visible promise about how patient the platform is.
func TestBackoffDelay(t *testing.T) {
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, 16 * time.Minute},
		{5, 30 * time.Minute},
		{9, 30 * time.Minute}, // capped: broken, not gone
	} {
		if got := backoffDelay(tc.failures); got != tc.want {
			t.Errorf("backoffDelay(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

// TestBackoff_SkipsUntilDueAndClearsOnChange — the two ways a stack escapes the delay: time passing,
// or the operator touching the stack (a deploy or `stack sync` bumps updated_at). The second matters:
// waiting out a 30-minute backoff after you have just fixed the cause is the kind of "the platform is
// ignoring me" that erodes trust.
func TestBackoff_SkipsUntilDueAndClearsOnChange(t *testing.T) {
	r := &Reconciler{}
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	st := &store.Stack{Name: "aarma", CurrentRevision: 10, UpdatedAt: 1000}

	// Nothing recorded: not in backoff.
	if waiting, _ := r.inBackoff(st, now); waiting {
		t.Fatal("a stack with no failures is in backoff")
	}

	wait := r.scheduleBackoff(st, now)
	if wait != 2*time.Minute {
		t.Fatalf("first failure waits %v, want 2m", wait)
	}
	if waiting, left := r.inBackoff(st, now.Add(30*time.Second)); !waiting || left <= 0 {
		t.Fatalf("a stack inside its delay is not skipped (waiting=%v left=%v)", waiting, left)
	}
	if waiting, _ := r.inBackoff(st, now.Add(3*time.Minute)); waiting {
		t.Error("still skipped after the delay expired")
	}

	// Operator action: the row changed, so try now.
	r.scheduleBackoff(st, now)
	touched := &store.Stack{Name: "aarma", CurrentRevision: 10, UpdatedAt: 2000}
	if waiting, _ := r.inBackoff(touched, now.Add(10*time.Second)); waiting {
		t.Error("a stack the operator touched is still backing off")
	}

	// Success clears it.
	r.scheduleBackoff(st, now)
	r.clearBackoff("aarma")
	if waiting, _ := r.inBackoff(st, now.Add(time.Second)); waiting {
		t.Error("backoff survived a successful sync")
	}
}

// TestBackoff_GrowsPerFailure — consecutive failures must delay further, or a crash-looping stack
// costs a two-minute wait every couple of minutes forever.
func TestBackoff_GrowsPerFailure(t *testing.T) {
	r := &Reconciler{}
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	st := &store.Stack{Name: "demo", UpdatedAt: 1000}

	var got []time.Duration
	for i := 0; i < 3; i++ {
		got = append(got, r.scheduleBackoff(st, now))
	}
	if got[0] != 2*time.Minute || got[1] != 4*time.Minute || got[2] != 8*time.Minute {
		t.Fatalf("ladder = %v, want 2m, 4m, 8m", got)
	}
}
