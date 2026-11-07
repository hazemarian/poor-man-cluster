package views

import (
	"bytes"
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

// TestRenderedLogRowsCarryTheirTimestamp renders the rows-only template through
// the real renderer — the same parsed template set the console serves — and
// asserts what the incremental poll depends on: every row carries data-ts, and
// the fragment contains no panel wrapper for the next poll to nest inside.
func TestRenderedLogRowsCarryTheirTimestamp(t *testing.T) {
	type row struct {
		Timestamp string
		TimeShort string
		SevClass  string
		Severity  string
		Service   string
		Body      string
		Preview   string
		Multiline bool
	}
	type evidence struct{ Rows []row }
	data := struct{ Evidence evidence }{Evidence: evidence{Rows: []row{
		{Timestamp: "2026-11-07T10:00:00Z", TimeShort: "10:00:00", SevClass: "bad", Severity: "error", Service: "svc", Body: "boom"},
		{Timestamp: "2026-11-07T10:00:05Z", TimeShort: "10:00:05", SevClass: "plain", Service: "svc", Body: "fine"},
	}}}

	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	var buf bytes.Buffer
	if err := r.tmpl.ExecuteTemplate(&buf, "observabilitylogrows", data); err != nil {
		t.Fatalf("render rows: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`class="obs-log"`,
		`data-ts="2026-11-07T10:00:00Z"`,
		`data-ts="2026-11-07T10:00:05Z"`,
		"boom",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered rows missing %s\n%s", want, out)
		}
	}
	if strings.Contains(out, `id="obs-logs"`) {
		t.Error("the incremental fragment rendered the panel wrapper; the next poll would nest a second #obs-logs")
	}
}
