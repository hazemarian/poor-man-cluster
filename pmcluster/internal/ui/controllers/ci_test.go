package controllers

import (
	"net/http"
	"strings"
	"testing"
)

// The CI board exists because a queue you cannot see is indistinguishable from a
// broken pipeline. Its tests pin the two things that make it trustworthy: every
// state an operator can act on is rendered distinctly, and the two rows that would
// otherwise mislead — an unreadable repo and a cancelled-but-superseded run — say
// what they are.
func mountCI(h *ctrlHarness) {
	h.engine.GET("/web/ci", (CI{Controller: h.ctrl}).Page)
}

func TestControllers_CIBoardRendersEveryState(t *testing.T) {
	h := newCtrlHarness(t)
	mountCI(h)
	rr := h.get(t, "/web/ci")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /web/ci = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	for _, want := range []string{
		"acme/api",              // the failing repo
		"acme/web",              // the queued one
		"acme/db",               // the cancelled/superseded one
		"acme/secret",           // the unreadable one
		"deploy → Deploy stack", // the failing step: the answer that needed a trip to GitHub
		"superseded",            // inferred, and labelled
		"not readable",          // a row, not a gap
		">open<",                // a way out to the run itself (the dictionary says "open")
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the board does not contain %q", want)
		}
	}
}

// The header counts must match the table, or the panel contradicts itself.
func TestControllers_CIBoardCountsMatchTheRows(t *testing.T) {
	h := newCtrlHarness(t)
	mountCI(h)
	body := h.get(t, "/web/ci").Body.String()
	if !strings.Contains(body, `>1<`) {
		t.Errorf("the queued/failed counts are not rendered: %s", firstLine(body))
	}
}

// "superseded" is inference and must be marked as such — also in the footnote, so
// the asterisk is explainable.
func TestControllers_CIBoardLabelsInference(t *testing.T) {
	h := newCtrlHarness(t)
	mountCI(h)
	body := h.get(t, "/web/ci").Body.String()
	if !strings.Contains(body, "inferred") {
		t.Fatalf("the board presents an inferred state without saying so: %s", firstLine(body))
	}
}
