package cluster

import (
	"bytes"
	"strings"
	"testing"
)

// The sink is the only reason an unattended reconcile's degradation is ever
// seen by a human, so what it keeps (and what it refuses to keep) is contract.
func TestWarningSinkKeepsOnlyWarningLines(t *testing.T) {
	var out bytes.Buffer
	w := &warningSink{dst: &out}

	// A realistic run: step headers, successes, one warning, more steps.
	for _, line := range []string{
		"▶ Repairing swarm configs/secrets from the DB index\n",
		"  ⚠ secret app_session_secret: cannot rebuild swarm object\n",
		"✓ 3 configs applied\n",
	} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	got := w.List()
	if len(got) != 1 {
		t.Fatalf("kept %d warnings, want 1: %#v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "secret app_session_secret") {
		t.Errorf("the marker and padding should be stripped: %q", got[0])
	}
	if out.String() != "▶ Repairing swarm configs/secrets from the DB index\n"+
		"  ⚠ secret app_session_secret: cannot rebuild swarm object\n"+
		"✓ 3 configs applied\n" {
		t.Errorf("the sink must not alter the run output:\n%s", out.String())
	}
}

// A per-node loop emits the same warning once per node. The count is not the
// information, so consecutive duplicates collapse to one entry.
func TestWarningSinkCollapsesConsecutiveDuplicates(t *testing.T) {
	var out bytes.Buffer
	w := &warningSink{dst: &out}
	for i := 0; i < 5; i++ {
		w.Write([]byte("  ⚠ could not clear the storage label\n"))
	}
	if got := w.List(); len(got) != 1 {
		t.Fatalf("kept %d entries for one repeated warning: %#v", len(got), got)
	}
}

// A warning without a trailing newline (the last line of a failed run) still
// has to be recorded — that is often the one that explains the failure.
func TestWarningSinkFlushesATrailingPartialLine(t *testing.T) {
	var out bytes.Buffer
	w := &warningSink{dst: &out}
	w.Write([]byte("  ⚠ could not record the platform apply: database is locked"))
	got := w.List()
	if len(got) != 1 || !strings.Contains(got[0], "database is locked") {
		t.Fatalf("trailing warning lost: %#v", got)
	}
}

// The record is bounded, and it says so rather than implying it holds
// everything.
func TestWarningSinkCapsAndSaysSo(t *testing.T) {
	var out bytes.Buffer
	w := &warningSink{dst: &out}
	for i := 0; i < maxRecordedWarnings+5; i++ {
		w.Write([]byte("  ⚠ distinct warning " + itoa(i) + "\n"))
	}
	got := w.List()
	if len(got) != maxRecordedWarnings+1 {
		t.Fatalf("kept %d entries, want %d + a trailer", len(got), maxRecordedWarnings)
	}
	if last := got[len(got)-1]; !strings.Contains(last, "+5 more") {
		t.Errorf("the trailer should count what was dropped: %q", last)
	}
}
