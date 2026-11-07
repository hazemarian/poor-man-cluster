package views

import (
	"io/fs"
	"strings"
	"testing"
)

// TestLiveLogPollIsIncrementalAndPanelPreserving pins the four attributes that
// make the live logs panel cheap. Each one, dropped on its own, turns the poll
// back into a full-window re-query every five seconds:
//
//   - the hidden-tab guard, without which a backgrounded console keeps asking;
//   - the search-focus guard, without which it fights the operator typing;
//   - hx-vals/data-ts, without which it re-reads the whole window instead of
//     the rows that arrived since the newest one on screen;
//   - the row-body target and beforeend swap, without which the panel is
//     replaced wholesale and the scroll position and filters are lost.
//
// This is a source assertion rather than a rendered one: the panel needs a full
// observabilityData from the controller, and a render test would be a wiring
// test in disguise. A source guard fails loudly on the edit that removes it,
// which is the actual regression being protected against.
func TestLiveLogPollIsIncrementalAndPanelPreserving(t *testing.T) {
	b, err := fs.ReadFile(files, "templates/frag_observability.html")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	src := string(b)

	for _, want := range []struct{ needle, why string }{
		{"[!document.hidden", "a hidden tab must not poll Parseable"},
		{"document.activeElement.id !== 'obs-search'", "no polling while the operator types"},
		{"hx-vals='js:{since:", "the poll must send the newest timestamp it holds"},
		{"hx-target=\"#obs-logs .obs-logs\"", "new rows go into the list, not over the panel"},
		{"hx-swap=\"beforeend\"", "rows are appended (oldest-first listing), never a panel swap"},
		{`data-ts="{{.Timestamp}}"`, "each row must carry the timestamp the next poll sends as since"},
		{`{{define "observabilitylogrows"}}`, "the incremental poll renders rows only"},
	} {
		if !strings.Contains(src, want.needle) {
			t.Errorf("missing %q — %s", want.needle, want.why)
		}
	}

	// The incremental response is rows only: the panel must not be re-rendered
	// by a poll, so the rows template cannot reference the section it lives in.
	start := strings.Index(src, `{{define "observabilitylogrows"}}`)
	if start < 0 {
		t.Fatal("the rows-only template is not defined at all")
	}
	rows := src[start:]
	if i := strings.Index(rows, "{{end}}"); i >= 0 {
		rows = rows[:i]
	}
	if strings.Contains(rows, "id=\"obs-logs\"") {
		t.Error("the rows-only template must not render the panel wrapper")
	}
}
