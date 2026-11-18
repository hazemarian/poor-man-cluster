package store

import (
	"context"
	"strings"
	"testing"
)

// TestPruneDuplicateStackErrors is the cleanup for histories written BEFORE failure messages were
// canonical: the depends_on level list came from a map iteration, so one timeout alternated between
// "[db, web]" and "[web, db]" and every reconcile pass added an entry. Measured on this cluster, one
// stack carried twenty entries for a single broken deploy.
func TestPruneDuplicateStackErrors(t *testing.T) {
	ctx := context.Background()
	st := openStackErrStore(t)

	// Seven retries of one failure, written the way the old code wrote them: alternating name order.
	msgs := []string{
		"Deploying stack to the swarm: timed out after 2m0s waiting for depends_on level [db, web] to become ready",
		"Deploying stack to the swarm: timed out after 2m0s waiting for depends_on level [web, db] to become ready",
		"Deploying stack to the swarm: timed out after 2m0s waiting for depends_on level [db, web] to become ready",
		"Deploying stack to the swarm: timed out after 2m0s waiting for depends_on level [web, db] to become ready",
		"Deploying stack to the swarm: timed out after 2m0s waiting for depends_on level [db, web] to become ready",
		"Deploying stack to the swarm: timed out after 2m0s waiting for depends_on level [web, db] to become ready",
		"Deploying stack to the swarm: timed out after 2m0s depending on level [db, web] to become ready",
	}
	for i, m := range msgs {
		if _, err := st.RecordStackError(ctx, "demo", 100, m); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	// A different failure on a different revision must survive untouched.
	if _, err := st.RecordStackError(ctx, "demo", 99, "image pull failed"); err != nil {
		t.Fatalf("record other: %v", err)
	}

	before, err := st.ListStackErrors(ctx, "demo", 0)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}
	if len(before) != 8 {
		t.Fatalf("fixture = %d entries, want 8 (7 alternations + 1 other)", len(before))
	}

	removed, kept, err := st.PruneDuplicateStackErrors(ctx, "demo")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	// Six of the seven are the same failure with the names in different order → one entry; the
	// seventh says "depending on level" and is a genuinely different message; the image pull is a
	// different failure. So five entries disappear and three remain.
	if removed != 5 || kept != 3 {
		t.Fatalf("prune removed %d kept %d, want 5 and 3", removed, kept)
	}

	after, err := st.ListStackErrors(ctx, "demo", 0)
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if len(after) != 3 {
		t.Fatalf("after = %d entries, want 3: %+v", len(after), after)
	}
	// The seven retries became ONE entry carrying seven attempts, with the canonical wording.
	var merged *StackErrorEntry
	for i := range after {
		if after[i].Revision == 100 {
			merged = &after[i]
		}
	}
	if merged == nil {
		t.Fatal("the merged entry is gone")
	}
	if merged.Count != 6 {
		// The 7th message differs ("depending on level", no "waiting for") and is a real distinct
		// failure, so six merges into one and the seventh stays its own entry.
		t.Errorf("merged count = %d, want 6", merged.Count)
	}
	// The kept message is the NEWEST attempt's, verbatim — canonicalisation is only for the
	// comparison key, so an operator still reads the wording the platform actually produced
	// (including any diagnosis a deploy appended).
	if !strings.Contains(merged.Error, "depends_on level") {
		t.Errorf("kept message lost its shape: %q", merged.Error)
	}
	if canonicalStackError(merged.Error) == "" {
		t.Errorf("canonical form of the kept message is empty: %q", merged.Error)
	}
	if other := after[0]; other.Revision == 99 && other.Error != "image pull failed" {
		t.Errorf("unrelated failure was rewritten: %+v", other)
	}

	// Idempotent: a second pass finds nothing to do.
	removed2, kept2, err := st.PruneDuplicateStackErrors(ctx, "demo")
	if err != nil {
		t.Fatalf("prune again: %v", err)
	}
	if removed2 != 0 || kept2 != 3 {
		t.Errorf("second prune removed %d kept %d, want 0 and 3", removed2, kept2)
	}
}

// TestCanonicalStackError_HandlesBothWordings — the deploy timeout was renamed from "depends_on level
// [a, b] ... to become ready" to "waiting for [a] to become healthy". A comparison that knew only one
// spelling would let the same failure pile up again under two names, and the history is full of the old
// one.
func TestCanonicalStackError_HandlesBothWordings(t *testing.T) {
	oldForm := "Deploying stack to the swarm: timed out after 2m0s waiting for depends_on level [web, db] to become ready"
	newForm := "Deploying stack to the swarm: timed out after 2m0s waiting for [web, db] to become healthy"

	if canonicalStackError(oldForm) == oldForm {
		t.Error("the old wording was not normalised")
	}
	if canonicalStackError(newForm) == newForm {
		t.Error("the new wording was not normalised")
	}
	// Same failure, same diagnosis appended or not: one key.
	mixed := []string{
		newForm + " — web: 0/1 running, last failed task failed (\"starting\")",
		newForm + " — web: 0/1 running, last failed task failed (\"exited 1\")",
		newForm,
	}
	first := canonicalStackError(mixed[0])
	for _, m := range mixed[1:] {
		if got := canonicalStackError(m); got != first {
			t.Errorf("canonicalStackError(%q) = %q, want %q", m, got, first)
		}
	}
}

// TestCanonicalStackError_LeavesOtherMessagesAlone — the canonical form touches ONE message shape.
func TestCanonicalStackError_LeavesOtherMessagesAlone(t *testing.T) {
	for _, msg := range []string{
		"image pull failed",
		"run-once job demo_migrate failed: exit 1",
		"Deploying stack to the swarm: docker stack deploy: exit status 1",
	} {
		if got := canonicalStackError(msg); got != msg {
			t.Errorf("canonicalStackError(%q) = %q, want it unchanged", msg, got)
		}
	}
}
