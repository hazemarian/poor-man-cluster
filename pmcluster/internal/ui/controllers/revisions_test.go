package controllers

import (
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// TestRevisionRows is the console side of "one broken deploy, not twenty":
// a failure on the CURRENT revision is state, a failure on an older revision is
// history, and a collapsed retry count must survive into the row.
func TestRevisionRows(t *testing.T) {
	det := &pmapi.StackDetail{
		Stack: pmapi.Stack{Name: "demo", CurrentRevision: 3},
		Revisions: []pmapi.RevisionMeta{
			// The current deploy is failing, and the daemon collapsed 7 identical
			// retries into it.
			{Revision: 3, CreatedAt: 300, Error: "timed out waiting for [db, web]", Attempts: 7, FailedAt: 900},
			// An old revision whose deploy failed once, from a daemon that predates
			// Attempts/FailedAt: both must default rather than render as zero.
			{Revision: 2, CreatedAt: 200, Error: "boom"},
			// A clean apply.
			{Revision: 1, CreatedAt: 100},
		},
	}

	rows := revisionRows(det)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}

	cur := rows[0]
	if !cur.Current || cur.Error == "" || cur.Attempts != 7 || cur.FailedAt != 900 {
		t.Errorf("current row = %+v, want current+error+attempts 7+failedAt 900", cur)
	}

	old := rows[1]
	if old.Current {
		t.Error("revision 2 reported as current")
	}
	if old.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 for a daemon that sent no count", old.Attempts)
	}
	if old.FailedAt != old.CreatedAt {
		t.Errorf("failedAt = %d, want the revision timestamp %d", old.FailedAt, old.CreatedAt)
	}

	clean := rows[2]
	if clean.Error != "" || clean.Attempts != 0 {
		t.Errorf("clean row = %+v, want no error and no attempts", clean)
	}
}

// TestRevisionRows_NilDetailIsEmpty — a failed detail read must not panic the page.
func TestRevisionRows_NilDetailIsEmpty(t *testing.T) {
	if got := revisionRows(nil); len(got) != 0 {
		t.Fatalf("rows = %d, want 0", len(got))
	}
}

// TestNewestStackError pins the banner rule this work must not disturb: only the
// CURRENT revision's failure is surfaced as state.
func TestNewestStackError_OnlyTheCurrentRevision(t *testing.T) {
	history := []pmapi.StackError{
		{Revision: 7, Error: "old failure"},
		{Revision: 5, Error: "older failure"},
	}
	if got := newestStackError(history, 8); got != "" {
		t.Errorf("banner = %q, want none: the failures belong to older revisions", got)
	}
	current := []pmapi.StackError{{Revision: 8, Error: "timed out"}}
	if got := newestStackError(current, 8); got != "timed out" {
		t.Errorf("banner = %q, want the current revision's failure", got)
	}
}
